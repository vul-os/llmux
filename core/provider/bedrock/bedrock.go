// Package bedrock adapts AWS Bedrock's Anthropic Claude models to the canonical
// OpenAI interface. It translates OpenAI chat requests into the Bedrock-flavoured
// Anthropic Messages schema, signs the request with AWS Signature V4 (pure-Go,
// stdlib only), POSTs to the model's /invoke endpoint, and maps the
// Anthropic-shaped response back to OpenAI shape.
//
// Configuration:
//   - Region comes from ProviderConfig.Headers["region"] (default "us-east-1").
//   - BaseURL defaults to https://bedrock-runtime.{region}.amazonaws.com.
//   - AWS credentials are read from the environment: AWS_ACCESS_KEY_ID,
//     AWS_SECRET_ACCESS_KEY, and the optional AWS_SESSION_TOKEN.
//
// IMPORTANT: the request/response mapping is written to the *documented*
// Bedrock+Anthropic schema and is UNVERIFIED against the live service.
package bedrock

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

const (
	defaultRegion = "us-east-1"
	awsService    = "bedrock"
)

// genID returns an OpenAI-style chat completion id (used for synthesized stream
// chunks when the upstream id is absent).
func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// Provider implements provider.Provider for AWS Bedrock (Anthropic Claude).
type Provider struct {
	name    string
	baseURL string
	region  string
	headers map[string]string
}

// New builds a Bedrock provider from config. Region is taken from
// Headers["region"] (default us-east-1); BaseURL defaults to the regional
// bedrock-runtime endpoint.
func New(c config.ProviderConfig) *Provider {
	region := c.Headers["region"]
	if region == "" {
		region = defaultRegion
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://bedrock-runtime." + region + ".amazonaws.com"
	}
	return &Provider{name: c.Name, baseURL: base, region: region, headers: c.Headers}
}

// Name implements Provider.
func (p *Provider) Name() string { return p.name }

// envCredentials reads AWS credentials from the environment.
func envCredentials() credentials {
	return credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
}

// invokeURL builds the /model/{target}/invoke endpoint URL.
//
// The model id is a single path segment, so it is percent-encoded with the AWS
// RFC 3986 rules INCLUDING slashes. This matters for inference-profile and
// imported-model ARNs (e.g.
// "arn:aws:bedrock:...:inference-profile/us.anthropic.claude...-v2:0"), whose
// colons and slashes must be encoded ("%3A"/"%2F") or Bedrock's router 400s.
// url.PathEscape leaves ":" literal, which is why awsURIEncode is used instead.
func (p *Provider) invokeURL(target string) string {
	return p.baseURL + "/model/" + awsURIEncode(target, true) + "/invoke"
}

// post builds, signs, and sends the invoke request.
func (p *Provider) post(ctx context.Context, client *http.Client, target string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.invokeURL(target), bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range p.headers {
		if k == "region" {
			continue
		}
		req.Header.Set(k, v)
	}

	signRequest(req, body, envCredentials(), p.region, awsService, time.Now())

	resp, err := client.Do(req)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	return resp, nil
}

func (p *Provider) errorFromResponse(ctx context.Context, resp *http.Response) *provider.Error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	provider.SinkFrom(ctx).Capture(resp.Header)
	resp.Body.Close()
	e := &provider.Error{StatusCode: resp.StatusCode, Provider: p.name}

	msg := parseBedrockErrorMessage(data)
	// AWS surfaces the error kind in the X-Amzn-ErrorType header (e.g.
	// "ThrottlingException:..."); use it to fill an empty body.
	amznType := resp.Header.Get("X-Amzn-ErrorType")
	if i := strings.IndexAny(amznType, ":/"); i >= 0 {
		amznType = amznType[:i]
	}
	if msg == "" {
		if amznType != "" {
			msg = amznType
		} else {
			msg = fmt.Sprintf("upstream error (status %d)", resp.StatusCode)
		}
	}

	typ := provider.NormalizeErrorType(resp.StatusCode)
	e.Body = openai.NewError(msg, typ, bedrockContextLengthCode(resp.StatusCode, msg))
	return e
}

// parseBedrockErrorMessage extracts a human-readable message from an AWS/Bedrock
// error body. AWS puts the message under "message" or capital "Message";
// Anthropic-on-Bedrock surfaces {"type":"error","error":{"message":"..."}}. It
// returns "" when no message is found.
func parseBedrockErrorMessage(data []byte) string {
	var generic struct {
		Message    string `json:"message"`
		MessageCap string `json:"Message"`
		Error      struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &generic) != nil {
		return ""
	}
	switch {
	case generic.Error.Message != "":
		return generic.Error.Message
	case generic.Message != "":
		return generic.Message
	case generic.MessageCap != "":
		return generic.MessageCap
	default:
		return ""
	}
}

// bedrockContextLengthCode returns "context_length_exceeded" when the status and
// upstream message indicate the request exceeded the model's context window;
// otherwise it returns the empty string.
func bedrockContextLengthCode(status int, message string) string {
	if status != http.StatusBadRequest && status != http.StatusRequestEntityTooLarge {
		return ""
	}
	m := strings.ToLower(message)
	if !strings.Contains(m, "context") && !strings.Contains(m, "token") {
		return ""
	}
	for _, kw := range []string{"length", "window", "maximum", "too long"} {
		if strings.Contains(m, kw) {
			return "context_length_exceeded"
		}
	}
	return ""
}

// ensureAnthropic gates the adapter to Anthropic Claude models. This adapter
// emits an Anthropic Messages body via InvokeModel, so sending a non-Anthropic
// id (Nova/Llama/Titan) would silently produce a wrong body. Inference-profile
// ARNs such as "us.anthropic.claude-..." contain "anthropic" and pass.
func (p *Provider) ensureAnthropic(target string) *provider.Error {
	if strings.Contains(strings.ToLower(target), "anthropic") {
		return nil
	}
	return &provider.Error{
		StatusCode: http.StatusBadRequest,
		Provider:   p.name,
		Body: openai.NewError(
			fmt.Sprintf("bedrock adapter currently supports Anthropic Claude models only (got %q); Converse API for other families is planned", target),
			"invalid_request_error", ""),
	}
}

// ChatCompletion implements Provider.
func (p *Provider) ChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage) (*openai.ChatCompletionResponse, error) {
	if err := p.ensureAnthropic(target); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(toInvoke(req))

	resp, err := p.post(ctx, provider.DefaultHTTPClient, target, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()
	provider.SinkFrom(ctx).Capture(resp.Header)

	var ir invokeResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&ir); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return fromInvoke(&ir, req.Model), nil
}

// Embeddings implements Provider. The Bedrock Anthropic adapter has no
// embeddings API (mirrors the anthropic adapter); route embeddings elsewhere.
func (p *Provider) Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error) {
	return nil, &provider.Error{StatusCode: http.StatusNotImplemented, Provider: p.name,
		Body: openai.NewError("bedrock anthropic adapter does not provide an embeddings API; route embeddings to another provider", "invalid_request_error", "")}
}

// ChatCompletionStream implements Provider.
//
// v1 behaviour: this performs a single non-streaming /invoke and synthesizes the
// OpenAI chunk sequence (role delta, one content delta, optional tool_call
// deltas, a finish_reason chunk, and an optional usage chunk). It does NOT yet
// parse Bedrock's native binary event-stream framing
// (application/vnd.amazon.eventstream via /invoke-with-response-stream); wiring
// that decoder is a planned follow-up.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage, yield provider.ChunkFunc) error {
	if err := p.ensureAnthropic(target); err != nil {
		return err
	}
	body, _ := json.Marshal(toInvoke(req))

	resp, err := p.post(ctx, provider.DefaultHTTPClient, target, body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	var ir invokeResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&ir); err != nil {
		return provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}

	return p.synthesizeStream(&ir, req, yield)
}

// synthesizeStream emits a non-streaming invoke result as an OpenAI chunk
// sequence.
func (p *Provider) synthesizeStream(ir *invokeResponse, req *openai.ChatCompletionRequest, yield provider.ChunkFunc) error {
	full := fromInvoke(ir, req.Model)
	id := full.ID
	if id == "" {
		id = genID()
	}
	model := full.Model
	created := time.Now().Unix()

	emit := func(choice openai.ChunkChoice, usage *openai.Usage) error {
		chunk := &openai.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChunkChoice{choice}, Usage: usage,
		}
		if usage != nil {
			chunk.Choices = []openai.ChunkChoice{}
		}
		return yield(chunk)
	}

	msg := full.Choices[0].Message
	finish := full.Choices[0].FinishReason

	// 1. role delta
	if err := emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Role: "assistant"}}, nil); err != nil {
		return err
	}

	// 2. content delta (single chunk with the full text), if any
	if text := msg.Content.String(); text != "" {
		if err := emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Content: text}}, nil); err != nil {
			return err
		}
	}

	// 3. tool_call deltas, one chunk per tool call
	for i, tc := range msg.ToolCalls {
		idx := i
		if err := emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{
			ToolCalls: []openai.ToolCall{{
				Index: &idx, ID: tc.ID, Type: "function",
				Function: openai.FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			}},
		}}, nil); err != nil {
			return err
		}
	}

	// 4. finish_reason chunk
	fr := finish
	if fr == "" {
		fr = "stop"
	}
	if err := emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{}, FinishReason: &fr}, nil); err != nil {
		return err
	}

	// 5. optional usage chunk
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage && full.Usage != nil {
		if err := emit(openai.ChunkChoice{}, full.Usage); err != nil {
			return err
		}
	}
	return nil
}

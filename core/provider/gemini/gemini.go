// Package gemini adapts the Google Gemini generateContent API to the canonical
// OpenAI interface, including streaming via streamGenerateContent?alt=sse.
package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// Provider implements provider.Provider for Google Gemini.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
}

// New builds a Gemini provider from config.
func New(c config.ProviderConfig) *Provider {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://generativelanguage.googleapis.com/v1beta"
	}
	return &Provider{name: c.Name, baseURL: base, apiKey: c.ResolveKey(), headers: c.Headers}
}

// Name implements Provider.
func (p *Provider) Name() string { return p.name }

func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

func (p *Provider) endpoint(model, method string, sse bool) string {
	u := fmt.Sprintf("%s/models/%s:%s", p.baseURL, url.PathEscape(model), method)
	if sse {
		u += "?alt=sse"
	}
	return u
}

func (p *Provider) post(ctx context.Context, client *http.Client, endpoint string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", p.apiKey)
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	return resp, nil
}

func (p *Provider) errorFromResponse(ctx context.Context, resp *http.Response) *provider.Error {
	// Capture rate-limit/retry headers (e.g. on 429/5xx) before closing the body.
	provider.SinkFrom(ctx).Capture(resp.Header)
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	e := &provider.Error{StatusCode: resp.StatusCode, Provider: p.name}
	// Normalize the OpenAI error type from the HTTP status rather than relaying
	// Gemini's raw status string (RESOURCE_EXHAUSTED / INVALID_ARGUMENT / ...),
	// keeping the upstream message verbatim.
	typ := provider.NormalizeErrorType(resp.StatusCode)
	var env errorEnvelope
	if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
		code := ""
		if provider.IsContextLengthError(resp.StatusCode, env.Error.Message) {
			code = "context_length_exceeded"
		}
		e.Body = openai.NewError(env.Error.Message, typ, code)
	} else {
		e.Body = openai.NewError(fmt.Sprintf("upstream error (status %d)", resp.StatusCode), typ, "")
	}
	return e
}

// ChatCompletion implements Provider.
func (p *Provider) ChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage) (*openai.ChatCompletionResponse, error) {
	body, _ := json.Marshal(toGemini(req))
	resp, err := p.post(ctx, provider.DefaultHTTPClient, p.endpoint(target, "generateContent", false), body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()
	provider.SinkFrom(ctx).Capture(resp.Header)

	var gr generateResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&gr); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return fromGemini(&gr, req.Model, genID(), time.Now().Unix()), nil
}

// Embeddings implements Provider via Gemini's :embedContent (single input) and
// :batchEmbedContents (multiple inputs).
//
// The wire mapping follows the documented Gemini embeddings API and should be
// verified against live responses before relying on it in production.
func (p *Provider) Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error) {
	inputs, err := decodeEmbeddingInput(req.Input)
	if err != nil {
		return nil, &provider.Error{StatusCode: http.StatusBadRequest, Provider: p.name,
			Body: openai.NewError(err.Error(), "invalid_request_error", "")}
	}
	if len(inputs) == 0 {
		return nil, &provider.Error{StatusCode: http.StatusBadRequest, Provider: p.name,
			Body: openai.NewError("embeddings input must contain at least one string", "invalid_request_error", "")}
	}

	if len(inputs) == 1 {
		return p.embedSingle(ctx, req, target, inputs[0])
	}
	return p.embedBatch(ctx, req, target, inputs)
}

func (p *Provider) embedSingle(ctx context.Context, req *openai.EmbeddingRequest, target, text string) (*openai.EmbeddingResponse, error) {
	body, _ := json.Marshal(embedContentRequest{
		Content: embedContentPayload{Parts: []part{{Text: text}}},
	})
	resp, err := p.post(ctx, provider.DefaultHTTPClient, p.endpoint(target, "embedContent", false), body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	var er embedContentResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&er); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return &openai.EmbeddingResponse{
		Object: "list",
		Model:  req.Model,
		Data: []openai.EmbeddingData{{
			Object: "embedding", Index: 0, Embedding: er.Embedding.Values,
		}},
	}, nil
}

func (p *Provider) embedBatch(ctx context.Context, req *openai.EmbeddingRequest, target string, inputs []string) (*openai.EmbeddingResponse, error) {
	reqs := make([]batchEmbedItem, len(inputs))
	model := "models/" + target
	for i, text := range inputs {
		reqs[i] = batchEmbedItem{
			Model:   model,
			Content: embedContentPayload{Parts: []part{{Text: text}}},
		}
	}
	body, _ := json.Marshal(batchEmbedRequest{Requests: reqs})
	resp, err := p.post(ctx, provider.DefaultHTTPClient, p.endpoint(target, "batchEmbedContents", false), body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	var br batchEmbedResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&br); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	data := make([]openai.EmbeddingData, len(br.Embeddings))
	for i, e := range br.Embeddings {
		data[i] = openai.EmbeddingData{Object: "embedding", Index: i, Embedding: e.Values}
	}
	return &openai.EmbeddingResponse{Object: "list", Model: req.Model, Data: data}, nil
}

// ChatCompletionStream implements Provider.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage, yield provider.ChunkFunc) error {
	body, _ := json.Marshal(toGemini(req))
	resp, err := p.post(ctx, provider.StreamHTTPClient, p.endpoint(target, "streamGenerateContent", true), body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	st := &streamState{
		id: genID(), model: req.Model, created: time.Now().Unix(),
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
		yield:        yield,
	}
	if err := provider.ScanSSE(resp.Body, st.handle); err != nil {
		return err
	}
	return st.finishStream()
}

type streamState struct {
	id           string
	model        string
	created      int64
	includeUsage bool

	roleSent bool
	nextTool int
	usage    *usageMetadata
	finish   string

	yield provider.ChunkFunc
}

func (s *streamState) emit(choice openai.ChunkChoice) error {
	return s.yield(&openai.ChatCompletionChunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []openai.ChunkChoice{choice},
	})
}

func (s *streamState) handle(data []byte) error {
	var gr generateResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		return nil
	}
	if gr.UsageMetadata != nil {
		s.usage = gr.UsageMetadata
	}
	if len(gr.Candidates) == 0 {
		return nil
	}
	cand := gr.Candidates[0]

	if !s.roleSent {
		s.roleSent = true
		if err := s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Role: "assistant"}}); err != nil {
			return err
		}
	}

	var hasTool bool
	for _, pt := range cand.Content.Parts {
		if pt.Text != "" {
			if err := s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Content: pt.Text}}); err != nil {
				return err
			}
		}
		if pt.FunctionCall != nil {
			hasTool = true
			ti := s.nextTool
			s.nextTool++
			if err := s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{
				ToolCalls: []openai.ToolCall{{
					Index: &ti, ID: genID(), Type: "function",
					Function: openai.FunctionCall{Name: pt.FunctionCall.Name, Arguments: string(rawOrEmptyObject(string(pt.FunctionCall.Args)))},
				}},
			}}); err != nil {
				return err
			}
		}
	}
	if cand.FinishReason != "" {
		s.finish = mapFinishReason(cand.FinishReason, hasTool)
	}
	return nil
}

// finishStream emits the terminal finish_reason chunk and optional usage chunk.
func (s *streamState) finishStream() error {
	if !s.roleSent {
		// Nothing streamed: still produce a valid finish so clients terminate.
		s.roleSent = true
		_ = s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Role: "assistant"}})
	}
	fr := s.finish
	if fr == "" {
		fr = "stop"
	}
	if err := s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{}, FinishReason: &fr}); err != nil {
		return err
	}
	if s.includeUsage && s.usage != nil {
		return s.yield(&openai.ChatCompletionChunk{
			ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
			Choices: []openai.ChunkChoice{},
			Usage: &openai.Usage{
				PromptTokens: s.usage.PromptTokenCount, CompletionTokens: s.usage.CandidatesTokenCount,
				TotalTokens: s.usage.TotalTokenCount,
			},
		})
	}
	return nil
}

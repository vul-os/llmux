// Package cohere adapts the Cohere v2 Chat API to the canonical OpenAI
// interface: request/response translation, tool calling, and a full streaming
// state machine that emits OpenAI chunks.
//
// The wire mapping was written to the documented Cohere v2 /chat API and should
// be verified against live responses before relying on it in production.
package cohere

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// genID returns an OpenAI-style chat completion id.
func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// Provider implements provider.Provider for Cohere.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
}

// New builds a Cohere provider from config.
func New(c config.ProviderConfig) *Provider {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.cohere.com/v2"
	}
	return &Provider{name: c.Name, baseURL: base, apiKey: c.ResolveKey(), headers: c.Headers}
}

// Name implements Provider.
func (p *Provider) Name() string { return p.name }

func (p *Provider) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+p.apiKey)
	for k, v := range p.headers {
		r.Header.Set(k, v)
	}
}

func (p *Provider) post(ctx context.Context, client *http.Client, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat", bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	p.setHeaders(req)
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
	typ := provider.NormalizeErrorType(resp.StatusCode)
	var msg string
	var env errorEnvelope
	if json.Unmarshal(data, &env) == nil && env.Message != "" {
		msg = env.Message
	} else {
		msg = fmt.Sprintf("upstream error (status %d)", resp.StatusCode)
	}
	e.Body = openai.NewError(msg, typ, contextLengthCode(resp.StatusCode, msg))
	return e
}

// contextLengthCode returns "context_length_exceeded" when the status and
// upstream message indicate the request exceeded the model's context window;
// otherwise it returns the empty string (no specific OpenAI error code).
func contextLengthCode(status int, message string) string {
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

// ChatCompletion implements Provider.
func (p *Provider) ChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage) (*openai.ChatCompletionResponse, error) {
	creq, err := toCohere(req, target)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	creq.Stream = false
	body, _ := json.Marshal(creq)

	resp, err := p.post(ctx, provider.DefaultHTTPClient, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()
	provider.SinkFrom(ctx).Capture(resp.Header)

	var cr chatResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&cr); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return fromCohere(&cr, req.Model), nil
}

// Embeddings implements Provider via Cohere v2's /embed endpoint.
//
// The wire mapping follows the documented Cohere v2 /embed API and should be
// verified against live responses before relying on it in production.
func (p *Provider) Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error) {
	texts, err := decodeEmbeddingInput(req.Input)
	if err != nil {
		return nil, &provider.Error{StatusCode: http.StatusBadRequest, Provider: p.name,
			Body: openai.NewError(err.Error(), "invalid_request_error", "")}
	}
	if len(texts) == 0 {
		return nil, &provider.Error{StatusCode: http.StatusBadRequest, Provider: p.name,
			Body: openai.NewError("embeddings input must contain at least one string", "invalid_request_error", "")}
	}

	body, _ := json.Marshal(embedRequest{
		Model:          target,
		Texts:          texts,
		InputType:      "search_document",
		EmbeddingTypes: []string{"float"},
	})

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	p.setHeaders(hreq)
	resp, err := provider.DefaultHTTPClient.Do(hreq)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	var er embedResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&er); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	data := make([]openai.EmbeddingData, len(er.Embeddings.Float))
	for i, vec := range er.Embeddings.Float {
		data[i] = openai.EmbeddingData{Object: "embedding", Index: i, Embedding: vec}
	}
	return &openai.EmbeddingResponse{Object: "list", Model: req.Model, Data: data}, nil
}

// ChatCompletionStream implements Provider with a full SSE translation.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage, yield provider.ChunkFunc) error {
	creq, err := toCohere(req, target)
	if err != nil {
		return provider.NewTransportError(p.name, err)
	}
	creq.Stream = true
	body, _ := json.Marshal(creq)

	resp, err := p.post(ctx, provider.StreamHTTPClient, body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	st := &streamState{
		id:           genID(),
		model:        req.Model,
		created:      time.Now().Unix(),
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
		yield:        yield,
	}
	return provider.ScanSSE(resp.Body, st.handle)
}

// streamState drives the Cohere -> OpenAI streaming translation.
type streamState struct {
	id           string
	model        string
	created      int64
	includeUsage bool

	roleSent     bool
	nextTool     int // OpenAI tool index for the in-progress tool call
	inputTokens  int
	outputTokens int
	finish       string

	yield provider.ChunkFunc
}

func (s *streamState) emit(choice openai.ChunkChoice) error {
	return s.yield(&openai.ChatCompletionChunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []openai.ChunkChoice{choice},
	})
}

// emitRole emits the leading role=assistant delta exactly once.
func (s *streamState) emitRole() error {
	if s.roleSent {
		return nil
	}
	s.roleSent = true
	return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Role: "assistant"}})
}

func (s *streamState) handle(data []byte) error {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil // ignore unparseable keep-alives
	}
	switch ev.Type {
	case "message-start":
		return s.emitRole()

	case "content-start":
		return s.emitRole()

	case "content-delta":
		if ev.Delta == nil || ev.Delta.Message == nil || ev.Delta.Message.Content == nil {
			return nil
		}
		text := ev.Delta.Message.Content.Text
		if text == "" {
			return nil
		}
		if err := s.emitRole(); err != nil {
			return err
		}
		return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Content: text}})

	case "tool-call-start":
		if ev.Delta == nil || ev.Delta.Message == nil || ev.Delta.Message.ToolCalls == nil {
			return nil
		}
		if err := s.emitRole(); err != nil {
			return err
		}
		tc := ev.Delta.Message.ToolCalls
		ti := s.nextTool
		return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{
			ToolCalls: []openai.ToolCall{{
				Index: &ti, ID: tc.ID, Type: "function",
				Function: openai.FunctionCall{Name: tc.Function.Name},
			}},
		}})

	case "tool-call-delta":
		if ev.Delta == nil || ev.Delta.Message == nil || ev.Delta.Message.ToolCalls == nil {
			return nil
		}
		args := ev.Delta.Message.ToolCalls.Function.Arguments
		if args == "" {
			return nil
		}
		ti := s.nextTool
		return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{
			ToolCalls: []openai.ToolCall{{
				Index:    &ti,
				Function: openai.FunctionCall{Arguments: args},
			}},
		}})

	case "tool-call-end":
		s.nextTool++
		if s.finish == "" {
			s.finish = "tool_calls"
		}
		return nil

	case "message-end":
		if ev.Delta != nil {
			if ev.Delta.FinishReason != "" {
				s.finish = mapFinishReason(ev.Delta.FinishReason)
			}
			if ev.Delta.Usage != nil {
				s.inputTokens = ev.Delta.Usage.Tokens.InputTokens
				s.outputTokens = ev.Delta.Usage.Tokens.OutputTokens
			}
		}
		fr := s.finish
		if fr == "" {
			fr = "stop"
		}
		if err := s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{}, FinishReason: &fr}); err != nil {
			return err
		}
		if s.includeUsage {
			return s.yield(&openai.ChatCompletionChunk{
				ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
				Choices: []openai.ChunkChoice{},
				Usage: &openai.Usage{
					PromptTokens: s.inputTokens, CompletionTokens: s.outputTokens,
					TotalTokens: s.inputTokens + s.outputTokens,
				},
			})
		}
		return nil
	}
	return nil
}

// Package anthropic adapts the Anthropic Messages API to the canonical OpenAI
// interface: request/response translation, tool calling, vision, and a full
// streaming state machine that emits OpenAI chunks.
package anthropic

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

const defaultVersion = "2023-06-01"

// genID returns an OpenAI-style chat completion id.
func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// Provider implements provider.Provider for Anthropic.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	version string
	headers map[string]string
}

// New builds an Anthropic provider from config.
func New(c config.ProviderConfig) *Provider {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com/v1"
	}
	version := defaultVersion
	if v := c.Headers["anthropic-version"]; v != "" {
		version = v
	}
	return &Provider{name: c.Name, baseURL: base, apiKey: c.ResolveKey(), version: version, headers: c.Headers}
}

// Name implements Provider.
func (p *Provider) Name() string { return p.name }

func (p *Provider) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", p.apiKey)
	r.Header.Set("anthropic-version", p.version)
	for k, v := range p.headers {
		if k == "anthropic-version" {
			continue
		}
		r.Header.Set(k, v)
	}
}

func (p *Provider) post(ctx context.Context, client *http.Client, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
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
	// Capture rate-limit/retry headers (e.g. on 429/5xx) before closing the body.
	provider.SinkFrom(ctx).Capture(resp.Header)
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	e := &provider.Error{StatusCode: resp.StatusCode, Provider: p.name}
	// Always normalize the OpenAI error type from the HTTP status rather than
	// relaying the raw provider type, keeping the upstream message verbatim.
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
	areq, err := toAnthropic(req, target)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	areq.Stream = false
	body, _ := json.Marshal(areq)

	resp, err := p.post(ctx, provider.DefaultHTTPClient, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()
	provider.SinkFrom(ctx).Capture(resp.Header)

	var ar messagesResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&ar); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return fromAnthropic(&ar, req.Model), nil
}

// Embeddings implements Provider. Anthropic has no embeddings API.
func (p *Provider) Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error) {
	return nil, &provider.Error{StatusCode: http.StatusNotImplemented, Provider: p.name,
		Body: openai.NewError("anthropic does not provide an embeddings API; route embeddings to another provider", "invalid_request_error", "")}
}

// ChatCompletionStream implements Provider with a full SSE translation.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage, yield provider.ChunkFunc) error {
	areq, err := toAnthropic(req, target)
	if err != nil {
		return provider.NewTransportError(p.name, err)
	}
	areq.Stream = true
	body, _ := json.Marshal(areq)

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
		blockToTool:  map[int]int{},
		yield:        yield,
	}
	return provider.ScanSSE(resp.Body, st.handle)
}

// streamState drives the Anthropic -> OpenAI streaming translation.
type streamState struct {
	id           string
	model        string
	created      int64
	includeUsage bool

	blockToTool  map[int]int // Anthropic content-block index -> OpenAI tool index
	nextTool     int
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

func (s *streamState) handle(data []byte) error {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil // ignore unparseable keep-alives
	}
	switch ev.Type {
	case "message_start":
		var m struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage usage  `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &m) == nil {
			if m.Message.ID != "" {
				s.id = m.Message.ID
			}
			if m.Message.Model != "" {
				s.model = m.Message.Model
			}
			s.inputTokens = m.Message.Usage.InputTokens
		}
		return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Role: "assistant"}})

	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			ti := s.nextTool
			s.nextTool++
			s.blockToTool[ev.Index] = ti
			return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{
				ToolCalls: []openai.ToolCall{{
					Index: &ti, ID: ev.ContentBlock.ID, Type: "function",
					Function: openai.FunctionCall{Name: ev.ContentBlock.Name},
				}},
			}})
		}
		return nil

	case "content_block_delta":
		if ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return nil
			}
			return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{Content: ev.Delta.Text}})
		case "input_json_delta":
			ti := s.blockToTool[ev.Index]
			return s.emit(openai.ChunkChoice{Index: 0, Delta: openai.Delta{
				ToolCalls: []openai.ToolCall{{
					Index:    &ti,
					Function: openai.FunctionCall{Arguments: ev.Delta.PartialJSON},
				}},
			}})
		}
		return nil

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.finish = mapStopReason(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			s.outputTokens = ev.Usage.OutputTokens
		}
		return nil

	case "message_stop":
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

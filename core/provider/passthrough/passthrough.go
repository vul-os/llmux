// Package passthrough implements Provider for upstreams that already speak the
// OpenAI HTTP schema (OpenAI, DeepSeek, Groq, Mistral, Together, Fireworks, xAI,
// OpenRouter, Ollama/vLLM, ...). It forwards the request near-verbatim, swapping
// only the base URL, auth, and (if routed) the model name.
package passthrough

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// Provider forwards OpenAI-shaped requests to a configured upstream.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
}

// New builds a passthrough provider from config.
func New(c config.ProviderConfig) *Provider {
	return &Provider{
		name:    c.Name,
		baseURL: strings.TrimRight(c.BaseURL, "/"),
		apiKey:  c.ResolveKey(),
		headers: c.Headers,
	}
}

// Name implements Provider.
func (p *Provider) Name() string { return p.name }

func (p *Provider) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		r.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		r.Header.Set(k, v)
	}
}

func (p *Provider) post(ctx context.Context, client *http.Client, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
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

// errorFromResponse reads an error body and builds a provider.Error. It also
// captures rate-limit/retry-after headers (valuable on 429/5xx) into the sink.
func (p *Provider) errorFromResponse(ctx context.Context, resp *http.Response) *provider.Error {
	provider.SinkFrom(ctx).Capture(resp.Header)
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	e := &provider.Error{StatusCode: resp.StatusCode, Provider: p.name}
	var parsed openai.ErrorResponse
	if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
		e.Body = &parsed // structured provider error: relay its message
	} else {
		// Non-JSON body (e.g. an intermediary's HTML error): do not echo raw
		// upstream content to the client; report status only.
		e.Body = openai.NewError(fmt.Sprintf("upstream error (status %d)", resp.StatusCode), "upstream_error", "")
	}
	return e
}

// ChatCompletion implements Provider.
func (p *Provider) ChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage) (*openai.ChatCompletionResponse, error) {
	body := provider.SetJSONFields(raw, map[string]any{"model": target, "stream": false})
	resp, err := p.post(ctx, provider.DefaultHTTPClient, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()
	provider.SinkFrom(ctx).Capture(resp.Header)

	var out openai.ChatCompletionResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&out); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return &out, nil
}

// ChatCompletionStream implements Provider.
func (p *Provider) ChatCompletionStream(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage, yield provider.ChunkFunc) error {
	body := provider.SetJSONFields(raw, map[string]any{"model": target, "stream": true})

	resp, err := p.post(ctx, provider.StreamHTTPClient, "/chat/completions", body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()

	return provider.ScanSSE(resp.Body, func(data []byte) error {
		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			// Skip unparseable keep-alive/comment payloads rather than abort.
			return nil
		}
		return yield(&chunk)
	})
}

// Forward implements provider.Forwarder: it transparently proxies an
// OpenAI-shaped resource request to the upstream, swapping in provider auth.
func (p *Provider) Forward(ctx context.Context, fr provider.ForwardRequest) (*provider.ForwardResponse, error) {
	method := fr.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+fr.Suffix, bytes.NewReader(fr.Body))
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	ct := fr.ContentType
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := provider.StreamHTTPClient.Do(req)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	return &provider.ForwardResponse{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// Embeddings implements Provider.
func (p *Provider) Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error) {
	body := provider.SetJSONFields(raw, map[string]any{"model": target})
	resp, err := p.post(ctx, provider.DefaultHTTPClient, "/embeddings", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, p.errorFromResponse(ctx, resp)
	}
	defer resp.Body.Close()
	provider.SinkFrom(ctx).Capture(resp.Header)

	var out openai.EmbeddingResponse
	if err := json.NewDecoder(provider.Body(resp)).Decode(&out); err != nil {
		return nil, provider.NewTransportError(p.name, fmt.Errorf("decode response: %w", err))
	}
	return &out, nil
}

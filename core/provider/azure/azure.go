// Package azure adapts Azure OpenAI to the canonical OpenAI interface. Azure
// speaks the same request/response/stream schema as OpenAI, so this adapter is
// modeled on the passthrough provider; it differs only in (1) auth via the
// `api-key` header instead of `Authorization: Bearer`, (2) URLs of the form
// {baseURL}/openai/deployments/{deployment}/{op}?api-version={ver}, where the
// routed model name is the Azure *deployment* name, and (3) the api-version
// query parameter sourced from Headers["api-version"].
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// defaultAPIVersion is a recent stable Azure OpenAI API version, used when the
// provider config does not set Headers["api-version"].
const defaultAPIVersion = "2024-10-21"

// Provider forwards OpenAI-shaped requests to an Azure OpenAI resource.
type Provider struct {
	name       string
	baseURL    string
	apiKey     string
	apiVersion string
	headers    map[string]string
}

// New builds an Azure OpenAI provider from config. BaseURL is the Azure
// resource endpoint (e.g. https://myres.openai.azure.com). The api-version is
// read from Headers["api-version"], falling back to a recent stable default.
func New(c config.ProviderConfig) *Provider {
	ver := c.Headers["api-version"]
	if ver == "" {
		ver = defaultAPIVersion
	}
	return &Provider{
		name:       c.Name,
		baseURL:    strings.TrimRight(c.BaseURL, "/"),
		apiKey:     c.ResolveKey(),
		apiVersion: ver,
		headers:    c.Headers,
	}
}

// Name implements Provider.
func (p *Provider) Name() string { return p.name }

func (p *Provider) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		r.Header.Set("api-key", p.apiKey)
	}
	for k, v := range p.headers {
		// api-version is a query parameter, not a header; don't leak it.
		if strings.EqualFold(k, "api-version") {
			continue
		}
		r.Header.Set(k, v)
	}
}

// deploymentURL builds {baseURL}/openai/deployments/{deployment}/{op} with the
// api-version query, constructed query-safely via net/url.
func (p *Provider) deploymentURL(deployment, op string) string {
	u := fmt.Sprintf("%s/openai/deployments/%s/%s", p.baseURL, url.PathEscape(deployment), op)
	q := url.Values{"api-version": {p.apiVersion}}
	return u + "?" + q.Encode()
}

func (p *Provider) post(ctx context.Context, client *http.Client, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
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
	// Normalize the OpenAI error type from the HTTP status, keeping the upstream
	// message verbatim when it parses as a structured error.
	typ := provider.NormalizeErrorType(resp.StatusCode)
	var parsed openai.ErrorResponse
	if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
		code := parsed.Error.Code
		if provider.IsContextLengthError(resp.StatusCode, parsed.Error.Message) {
			code = "context_length_exceeded"
		}
		e.Body = openai.NewError(parsed.Error.Message, typ, code)
		e.Body.Error.Param = parsed.Error.Param
	} else {
		// Non-JSON body (e.g. an intermediary's HTML error): do not echo raw
		// upstream content to the client; report status only.
		e.Body = openai.NewError(fmt.Sprintf("upstream error (status %d)", resp.StatusCode), typ, "")
	}
	return e
}

// ChatCompletion implements Provider.
func (p *Provider) ChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage) (*openai.ChatCompletionResponse, error) {
	body := provider.SetJSONFields(raw, map[string]any{"model": target, "stream": false})
	resp, err := p.post(ctx, provider.DefaultHTTPClient, p.deploymentURL(target, "chat/completions"), body)
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

	resp, err := p.post(ctx, provider.StreamHTTPClient, p.deploymentURL(target, "chat/completions"), body)
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

// Embeddings implements Provider.
func (p *Provider) Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error) {
	body := provider.SetJSONFields(raw, map[string]any{"model": target})
	resp, err := p.post(ctx, provider.DefaultHTTPClient, p.deploymentURL(target, "embeddings"), body)
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

// Forward implements provider.Forwarder: it transparently proxies an
// OpenAI-shaped resource request to the Azure deployment, swapping in provider
// auth. fr.Suffix is the deployment-scoped resource path and already starts with
// "/" (e.g. "/myDeployment/chat/completions"); it is mounted under
// {baseURL}/openai/deployments with the api-version query appended.
func (p *Provider) Forward(ctx context.Context, fr provider.ForwardRequest) (*provider.ForwardResponse, error) {
	method := fr.Method
	if method == "" {
		method = http.MethodPost
	}
	rawURL := p.forwardURL(fr.Suffix)
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(fr.Body))
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	ct := fr.ContentType
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	if p.apiKey != "" {
		req.Header.Set("api-key", p.apiKey)
	}
	for k, v := range p.headers {
		if strings.EqualFold(k, "api-version") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := provider.StreamHTTPClient.Do(req)
	if err != nil {
		return nil, provider.NewTransportError(p.name, err)
	}
	return &provider.ForwardResponse{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// forwardURL maps a forward suffix (which already starts with "/", e.g.
// "/{deployment}/chat/completions") to the Azure deployment URL
// {baseURL}/openai/deployments{suffix}?api-version=... The api-version query is
// appended safely (never string-concatenated after an existing query string).
func (p *Provider) forwardURL(suffix string) string {
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	base := p.baseURL + "/openai/deployments" + suffix
	q := url.Values{"api-version": {p.apiVersion}}
	if strings.Contains(base, "?") {
		return base + "&" + q.Encode()
	}
	return base + "?" + q.Encode()
}

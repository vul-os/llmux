// Package provider defines the upstream-provider abstraction. Every provider —
// passthrough or adapter — implements Provider, and the server only ever speaks
// the canonical openai types. Provider-specific quirks stay behind this seam.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/vul-os/llmux/core/openai"
)

// ChunkFunc receives one streaming chunk. Returning an error aborts the stream.
type ChunkFunc func(*openai.ChatCompletionChunk) error

// Provider is an upstream LLM provider behind the canonical OpenAI interface.
type Provider interface {
	// Name is the configured provider name (e.g. "openai", "anthropic").
	Name() string

	// ChatCompletion performs a non-streaming completion. target is the
	// upstream model name after routing; raw is the original request body for
	// maximum fidelity (passthrough forwards it verbatim aside from the model).
	ChatCompletion(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage) (*openai.ChatCompletionResponse, error)

	// ChatCompletionStream performs a streaming completion, invoking yield for
	// each chunk in OpenAI chunk order.
	ChatCompletionStream(ctx context.Context, req *openai.ChatCompletionRequest, target string, raw json.RawMessage, yield ChunkFunc) error

	// Embeddings performs an embeddings request.
	Embeddings(ctx context.Context, req *openai.EmbeddingRequest, target string, raw json.RawMessage) (*openai.EmbeddingResponse, error)
}

// Registry holds the configured providers by name.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{byName: map[string]Provider{}} }

// Register adds a provider, returning an error on duplicate names.
func (r *Registry) Register(p Provider) error {
	if _, ok := r.byName[p.Name()]; ok {
		return fmt.Errorf("provider %q already registered", p.Name())
	}
	r.byName[p.Name()] = p
	return nil
}

// Get returns the named provider.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names returns the registered provider names.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	return out
}

// Error is a provider error that carries an HTTP status and an OpenAI-shaped
// body so the gateway can faithfully relay upstream failures to clients.
type Error struct {
	StatusCode int
	Body       *openai.ErrorResponse
	Provider   string
	Err        error
}

func (e *Error) Error() string {
	if e.Body != nil {
		return fmt.Sprintf("provider %s: %d: %s", e.Provider, e.StatusCode, e.Body.Error.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("provider %s: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("provider %s: status %d", e.Provider, e.StatusCode)
}

func (e *Error) Unwrap() error { return e.Err }

// Status returns the HTTP status to relay (defaults to 502 if unset).
func (e *Error) Status() int {
	if e.StatusCode != 0 {
		return e.StatusCode
	}
	return http.StatusBadGateway
}

// NewTransportError wraps a network/transport failure (no HTTP response). The
// client-facing body is generic — the underlying error (which can contain the
// outbound URL/host) is kept in Err for server-side logging only, never echoed.
func NewTransportError(provider string, err error) *Error {
	return &Error{StatusCode: http.StatusBadGateway, Provider: provider, Err: err,
		Body: openai.NewError("upstream request failed", "upstream_error", "")}
}

// noRedirect is a CheckRedirect hook that blocks HTTP redirect following.
// Provider endpoints must never redirect the gateway to another host: allowing
// automatic redirects opens an SSRF vector where a malicious or compromised
// upstream could redirect an outbound POST to an internal metadata service
// (e.g. 169.254.169.254). Returning http.ErrUseLastResponse makes the client
// return the 3xx response unchanged; the provider adapter then treats the
// non-200 status as an upstream error.
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// Options are the per-gateway knobs every adapter needs. They were package-level
// variables (DefaultHTTPClient, StreamHTTPClient, MaxResponseBytes, DropParams)
// until two gateways in one process could silently corrupt each other's settings:
// server.New MUTATED the globals, so the second gateway's config won for both.
// Options are now constructed per gateway and threaded into each adapter's New.
//
// The zero value is usable: nil clients fall back to freshly built defaults (see
// Client/Stream), MaxResponseBytes 0 means unlimited, and a nil DropParams drops
// nothing.
type Options struct {
	// MaxResponseBytes bounds non-streaming upstream response bodies. 0 = unlimited.
	MaxResponseBytes int64
	// DropParams lists request body fields to strip before forwarding to
	// OpenAI-shaped upstreams (the `drop_params` config). This is the gateway's
	// drop-unsupported-params lever: operators drop params a given fleet rejects
	// (e.g. logit_bias on some hosts) instead of surfacing upstream 400s.
	DropParams []string
	// HTTPClient performs non-streaming upstream calls. nil = the default client.
	HTTPClient *http.Client
	// StreamClient performs streaming upstream calls. nil = the default streaming
	// client (no client-level timeout; cancellation via context).
	StreamClient *http.Client
}

// NewOptions returns Options with freshly built HTTP clients, so each gateway
// owns its own connection pools and transport settings.
func NewOptions() Options {
	return Options{HTTPClient: NewHTTPClient(), StreamClient: NewStreamHTTPClient()}
}

// NewHTTPClient builds the client for non-streaming upstream calls. Redirects
// are blocked (see noRedirect) to prevent redirect-based SSRF.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: 600 * time.Second, CheckRedirect: noRedirect}
}

// NewStreamHTTPClient builds the streaming client. It has no client-level
// timeout so long streams aren't cut off; cancellation is driven by the request
// context. Redirects are blocked (see noRedirect) to prevent redirect-based SSRF.
func NewStreamHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: noRedirect}
}

// defaultClients are the process-wide fallbacks used only when Options carries
// no client of its own (the zero value, e.g. in a hand-built adapter or a test).
// They are never mutated from config — that is what Options is for.
var (
	defaultClient       = NewHTTPClient()
	defaultStreamClient = NewStreamHTTPClient()
)

// Client returns the client for non-streaming calls.
func (o Options) Client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return defaultClient
}

// Stream returns the client for streaming calls.
func (o Options) Stream() *http.Client {
	if o.StreamClient != nil {
		return o.StreamClient
	}
	return defaultStreamClient
}

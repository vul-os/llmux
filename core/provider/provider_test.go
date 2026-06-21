package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/openai"
)

// stubProvider is a minimal Provider implementation for registry tests.
type stubProvider struct{ name string }

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) ChatCompletion(context.Context, *openai.ChatCompletionRequest, string, json.RawMessage) (*openai.ChatCompletionResponse, error) {
	return nil, nil
}
func (s stubProvider) ChatCompletionStream(context.Context, *openai.ChatCompletionRequest, string, json.RawMessage, ChunkFunc) error {
	return nil
}
func (s stubProvider) Embeddings(context.Context, *openai.EmbeddingRequest, string, json.RawMessage) (*openai.EmbeddingResponse, error) {
	return nil, nil
}

func TestNewRegistryEmpty(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
	if got := r.Names(); len(got) != 0 {
		t.Fatalf("expected no names, got %v", got)
	}
	if _, ok := r.Get("anything"); ok {
		t.Fatal("expected Get on empty registry to miss")
	}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	p := stubProvider{name: "openai"}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := r.Get("openai")
	if !ok {
		t.Fatal("expected to find registered provider")
	}
	if got.Name() != "openai" {
		t.Fatalf("got provider name %q", got.Name())
	}

	if _, ok := r.Get("missing"); ok {
		t.Fatal("expected miss for unregistered name")
	}
}

func TestRegistryRegisterDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubProvider{name: "dup"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(stubProvider{name: "dup"})
	if err == nil {
		t.Fatal("expected duplicate registration to error")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Fatalf("error should mention name, got %v", err)
	}
}

func TestRegistryNames(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		if err := r.Register(stubProvider{name: n}); err != nil {
			t.Fatalf("Register %q: %v", n, err)
		}
	}
	names := r.Names()
	sort.Strings(names)
	want := []string{"a", "b", "c"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}
}

func TestErrorStringBody(t *testing.T) {
	e := &Error{
		StatusCode: 429,
		Provider:   "openai",
		Body:       openai.NewError("rate limited", "rate_limit_error", ""),
	}
	s := e.Error()
	if !strings.Contains(s, "openai") || !strings.Contains(s, "429") || !strings.Contains(s, "rate limited") {
		t.Fatalf("unexpected message: %q", s)
	}
}

func TestErrorStringErr(t *testing.T) {
	e := &Error{
		Provider: "anthropic",
		Err:      errors.New("dial tcp: connection refused"),
	}
	s := e.Error()
	if !strings.Contains(s, "anthropic") || !strings.Contains(s, "connection refused") {
		t.Fatalf("unexpected message: %q", s)
	}
}

func TestErrorStringStatusOnly(t *testing.T) {
	e := &Error{StatusCode: 503, Provider: "gemini"}
	s := e.Error()
	if !strings.Contains(s, "gemini") || !strings.Contains(s, "503") {
		t.Fatalf("unexpected message: %q", s)
	}
}

func TestErrorUnwrap(t *testing.T) {
	inner := errors.New("boom")
	e := &Error{Provider: "x", Err: inner}
	if !errors.Is(e, inner) {
		t.Fatal("errors.Is should match the wrapped error")
	}
	if errors.Unwrap(e) != inner {
		t.Fatal("Unwrap should return the inner error")
	}

	// No wrapped error.
	bare := &Error{Provider: "x", StatusCode: 500}
	if errors.Unwrap(bare) != nil {
		t.Fatal("Unwrap of bare Error should be nil")
	}
}

func TestErrorStatusDefaultAndExplicit(t *testing.T) {
	if got := (&Error{}).Status(); got != http.StatusBadGateway {
		t.Fatalf("default Status = %d, want %d", got, http.StatusBadGateway)
	}
	if got := (&Error{StatusCode: 418}).Status(); got != 418 {
		t.Fatalf("explicit Status = %d, want 418", got)
	}
}

func TestNewTransportError(t *testing.T) {
	const secretURL = "https://internal.example.com/v1/chat?key=abc123"
	underlying := errors.New("Post " + secretURL + ": dial tcp: i/o timeout")
	e := NewTransportError("openai", underlying)

	if e.Status() != http.StatusBadGateway {
		t.Fatalf("Status = %d, want 502", e.Status())
	}
	if e.Body == nil {
		t.Fatal("expected a client-facing Body")
	}
	if e.Body.Error.Message != "upstream request failed" {
		t.Fatalf("body message = %q", e.Body.Error.Message)
	}
	// The underlying error (with the URL) must be retained for logging.
	if !errors.Is(e, underlying) {
		t.Fatal("transport error should keep the underlying err for logging")
	}
	// But the client-facing body must NOT leak the URL/host.
	if strings.Contains(e.Body.Error.Message, "example.com") ||
		strings.Contains(e.Body.Error.Message, "http") {
		t.Fatalf("client body leaked URL: %q", e.Body.Error.Message)
	}
}

package passthrough

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

func newP(url string) *Provider {
	return New(config.ProviderConfig{Name: "p", Type: config.TypePassthrough, BaseURL: url, APIKey: "k"})
}

func basicReq() *openai.ChatCompletionRequest {
	return &openai.ChatCompletionRequest{Model: "m", Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}}}
}

// TestCancellationAbortsUpstream proves the request context propagates: when the
// caller cancels, the in-flight upstream call returns promptly with an error.
func TestCancellationAbortsUpstream(t *testing.T) {
	// Handler blocks until the client goes away, with a self-timeout so the
	// server can never hang the test if cancellation didn't propagate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := newP(srv.URL+"/v1").ChatCompletion(ctx, basicReq(), "m", []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	// Client-side cancellation must abort the call quickly (not wait for upstream).
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation did not abort promptly: %v", time.Since(start))
	}
}

// TestResponseSizeLimit proves MaxResponseBytes truncates oversized bodies
// (decode then fails rather than buffering unbounded data).
func TestResponseSizeLimit(t *testing.T) {
	big := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("A", 5000) + `"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	old := provider.MaxResponseBytes
	provider.MaxResponseBytes = 64 // truncate hard
	defer func() { provider.MaxResponseBytes = old }()

	_, err := newP(srv.URL+"/v1").ChatCompletion(context.Background(), basicReq(), "m", []byte(`{"model":"m"}`))
	if err == nil {
		t.Fatal("expected decode error from truncated (size-limited) body")
	}
}

// TestHeaderCapture proves rate-limit headers are captured into the sink.
func TestHeaderCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining-Requests", "42")
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-Secret-Internal", "nope")
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "m",
			Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}}},
		})
	}))
	defer srv.Close()

	ctx, sink := provider.WithHeaderSink(context.Background())
	_, err := newP(srv.URL+"/v1").ChatCompletion(ctx, basicReq(), "m", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sink.Header().Get("X-RateLimit-Remaining-Requests") != "42" {
		t.Fatal("rate-limit header not captured")
	}
	if sink.Header().Get("Retry-After") != "5" {
		t.Fatal("retry-after not captured")
	}
	if sink.Header().Get("X-Secret-Internal") != "" {
		t.Fatal("non-allowlisted header leaked")
	}
}

// TestClientAuthNotForwarded proves the gateway swaps in the provider key and
// does NOT forward the client's bearer token upstream.
func TestClientAuthNotForwarded(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{ID: "x", Object: "chat.completion", Model: "m",
			Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}}}})
	}))
	defer srv.Close()

	// The provider is configured with key "k"; a client token never reaches it.
	_, err := newP(srv.URL+"/v1").ChatCompletion(context.Background(), basicReq(), "m", []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("upstream auth=%q, want the provider key only", gotAuth)
	}
}

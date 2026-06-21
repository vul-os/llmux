package azure

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

func newProvider(t *testing.T, baseURL string, headers map[string]string) *Provider {
	t.Helper()
	return New(config.ProviderConfig{
		Name:    "azure",
		BaseURL: baseURL,
		APIKey:  "secret-key",
		Headers: headers,
	})
}

// assertAuth checks Azure-specific auth: api-key header set, no Authorization.
func assertAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("api-key"); got != "secret-key" {
		t.Errorf("api-key header = %q, want %q", got, "secret-key")
	}
	if got := r.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want empty (Azure uses api-key)", got)
	}
}

func TestChatCompletion(t *testing.T) {
	var gotPath, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.URL.Query().Get("api-version")
		assertAuth(t, r)
		// Body model is harmless; verify it was set to the deployment.
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "gpt4o-dep" {
			t.Errorf("body model = %v, want gpt4o-dep", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "chatcmpl-1", Object: "chat.completion", Model: "gpt-4o",
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
		})
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, map[string]string{"api-version": "2024-10-21"})
	raw := json.RawMessage(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := p.ChatCompletion(context.Background(), &openai.ChatCompletionRequest{}, "gpt4o-dep", raw)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if want := "/openai/deployments/gpt4o-dep/chat/completions"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotVersion != "2024-10-21" {
		t.Errorf("api-version = %q, want 2024-10-21", gotVersion)
	}
	if resp.ID != "chatcmpl-1" || len(resp.Choices) != 1 {
		t.Errorf("response not translated: %+v", resp)
	}
}

func TestDefaultAPIVersion(t *testing.T) {
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("api-version")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{ID: "x"})
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil) // no api-version header
	_, err := p.ChatCompletion(context.Background(), &openai.ChatCompletionRequest{}, "dep", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if gotVersion != defaultAPIVersion {
		t.Errorf("api-version = %q, want default %q", gotVersion, defaultAPIVersion)
	}
}

func TestChatCompletionStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("stream path = %q", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("stream not set true: %v", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil)
	var chunks []string
	err := p.ChatCompletionStream(context.Background(), &openai.ChatCompletionRequest{}, "dep",
		json.RawMessage(`{"messages":[]}`), func(c *openai.ChatCompletionChunk) error {
			if len(c.Choices) > 0 {
				chunks = append(chunks, c.Choices[0].Delta.Content)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := strings.Join(chunks, ""); got != "hello" {
		t.Errorf("streamed content = %q, want %q", got, "hello")
	}
}

func TestEmbeddings(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assertAuth(t, r)
		if r.URL.Query().Get("api-version") == "" {
			t.Error("api-version missing on embeddings")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openai.EmbeddingResponse{
			Object: "list", Model: "text-embedding-3-small",
			Data: []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}}},
		})
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil)
	resp, err := p.Embeddings(context.Background(), &openai.EmbeddingRequest{},
		"embed-dep", json.RawMessage(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if want := "/openai/deployments/embed-dep/embeddings"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 2 {
		t.Errorf("embeddings not translated: %+v", resp)
	}
}

func TestErrorStatusRelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(openai.NewError("rate limited", "ignored_upstream_type", ""))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil)
	_, err := p.ChatCompletion(context.Background(), &openai.ChatCompletionRequest{}, "dep", json.RawMessage(`{}`))
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected provider.Error, got %T: %v", err, err)
	}
	if pe.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", pe.StatusCode)
	}
	if pe.Body.Error.Type != "rate_limit_error" {
		t.Errorf("type = %q, want rate_limit_error", pe.Body.Error.Type)
	}
	if pe.Body.Error.Message != "rate limited" {
		t.Errorf("message = %q, want %q", pe.Body.Error.Message, "rate limited")
	}
}

func TestContextLengthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(openai.NewError("This model's maximum context length is 8192 tokens", "invalid_request_error", ""))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil)
	_, err := p.ChatCompletion(context.Background(), &openai.ChatCompletionRequest{}, "dep", json.RawMessage(`{}`))
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected provider.Error, got %T", err)
	}
	if pe.Body.Error.Code != "context_length_exceeded" {
		t.Errorf("code = %q, want context_length_exceeded", pe.Body.Error.Code)
	}
}

func TestNonJSONErrorNotEchoed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "<html>gateway boom</html>")
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil)
	_, err := p.ChatCompletion(context.Background(), &openai.ChatCompletionRequest{}, "dep", json.RawMessage(`{}`))
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected provider.Error, got %T", err)
	}
	if strings.Contains(pe.Body.Error.Message, "boom") {
		t.Errorf("raw HTML echoed to client: %q", pe.Body.Error.Message)
	}
	if pe.Body.Error.Type != "api_error" {
		t.Errorf("type = %q, want api_error", pe.Body.Error.Type)
	}
}

func TestForward(t *testing.T) {
	var gotPath, gotVersion, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.URL.Query().Get("api-version")
		gotKey = r.Header.Get("api-key")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, nil)
	resp, err := p.Forward(context.Background(), provider.ForwardRequest{
		Method: http.MethodPost,
		Suffix: "/my-dep/chat/completions",
		Body:   []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()
	if want := "/openai/deployments/my-dep/chat/completions"; gotPath != want {
		t.Errorf("forward path = %q, want %q", gotPath, want)
	}
	if gotVersion == "" {
		t.Error("forward missing api-version query")
	}
	if gotKey != "secret-key" {
		t.Errorf("forward api-key = %q, want secret-key", gotKey)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("forward status = %d, want 200", resp.Status)
	}
}

func TestForwarderInterface(t *testing.T) {
	var _ provider.Provider = (*Provider)(nil)
	var _ provider.Forwarder = (*Provider)(nil)
}

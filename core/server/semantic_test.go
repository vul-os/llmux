package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// TestSemanticCacheWiring proves the server builds the semantic cache, embeds
// prompts via the configured embeddings route, and serves a repeat prompt from
// cache. A deterministic mock embedder maps identical text to identical vectors.
func TestSemanticCacheWiring(t *testing.T) {
	var chatCalls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			var req struct {
				Input string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			// Deterministic 4-dim vector from the input bytes.
			var v [4]float64
			for i := 0; i < len(req.Input); i++ {
				v[i%4] += float64(req.Input[i])
			}
			json.NewEncoder(w).Encode(openai.EmbeddingResponse{
				Object: "list", Model: "mock-embed",
				Data: []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: v[:]}},
			})
		case "/v1/chat/completions":
			atomic.AddInt32(&chatCalls, 1)
			json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
				ID: "x", Object: "chat.completion", Model: "gpt-4o",
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
				Usage:   &openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			})
		}
	}))
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Cache:     config.CacheConfig{Semantic: true, EmbeddingModel: "mock-embed", SimilarityThreshold: 0.99},
	}
	s, _ := New(cfg)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"what is the capital of france"}]}`
	rec1 := post(s, body)
	if rec1.Code != 200 || rec1.Header().Get("X-LLMux-Cache") == "hit" {
		t.Fatalf("first call should miss: %d %q", rec1.Code, rec1.Header().Get("X-LLMux-Cache"))
	}
	rec2 := post(s, body)
	if rec2.Header().Get("X-LLMux-Cache") != "hit" {
		t.Fatalf("identical prompt should hit semantic cache")
	}
	if got := atomic.LoadInt32(&chatCalls); got != 1 {
		t.Fatalf("chat upstream called %d times, want 1 (semantic cache served repeat)", got)
	}
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/openai"
)

// tokenIdentity resolves each distinct bearer token to its own account id, with
// NO static key (Key == nil) — exactly the cp-resolved-principal shape. It lets a
// core test exercise cross-tenant cache isolation without importing integration/cp.
type tokenIdentity struct{}

func (tokenIdentity) Resolve(_ context.Context, token string) (Principal, bool) {
	if token == "" {
		return Principal{}, false
	}
	return Principal{Token: token, AccountID: "acct_" + token, Tier: "test"}, true
}

// distinctChatUpstream returns a chat upstream that emits a UNIQUE assistant
// message on every call (resp-1, resp-2, ...) so a cache leak is observable: if
// account B is served account A's cached content, B sees A's message instead of a
// fresh one. For semantic mode it also serves a deterministic embedding so
// identical prompt text embeds identically.
func distinctChatUpstream(calls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			var req struct {
				Input string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var v [4]float64
			for i := 0; i < len(req.Input); i++ {
				v[i%4] += float64(req.Input[i])
			}
			json.NewEncoder(w).Encode(openai.EmbeddingResponse{
				Object: "list", Model: "mock-embed",
				Data: []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: v[:]}},
			})
		case "/v1/chat/completions":
			n := atomic.AddInt32(calls, 1)
			json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
				ID: "x", Object: "chat.completion", Model: "gpt-4o",
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str(fmt.Sprintf("resp-%d", n))}, FinishReason: "stop"}},
				Usage:   &openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			})
		}
	}))
}

func contentOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp openai.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response: %s", rec.Body.String())
	}
	return resp.Choices[0].Message.Content.String()
}

// TestCacheCrossTenantIsolationCP proves that two cp-resolved accounts (no static
// key) never share a cache entry — for BOTH the exact-match and the semantic
// cache. Before the fix both accounts scoped to "" and account B was served
// account A's cached (and, with semantic, merely SIMILAR) content.
func TestCacheCrossTenantIsolationCP(t *testing.T) {
	cases := []struct {
		name  string
		cache config.CacheConfig
	}{
		{"exact", config.CacheConfig{Enabled: true, MaxEntries: 100}},
		{"semantic", config.CacheConfig{Semantic: true, EmbeddingModel: "mock-embed", SimilarityThreshold: 0.99, MaxEntries: 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			up := distinctChatUpstream(&calls)
			defer up.Close()

			cfg := &config.Config{
				Server:    config.ServerConfig{Addr: ":0"},
				Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"}},
				Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
				Cache:     tc.cache,
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			s.SetIdentity(tokenIdentity{})
			s.SetBudgetGate(stubBudget{})

			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"what is the capital of france"}]}`

			// Account A: first call misses and is cached as resp-1.
			recA1 := postKey(s, body, "A")
			if recA1.Code != 200 {
				t.Fatalf("A1 status=%d body=%s", recA1.Code, recA1.Body.String())
			}
			if recA1.Header().Get("X-LLMux-Cache") == "hit" {
				t.Fatal("A first call should miss")
			}
			aContent := contentOf(t, recA1)

			// Account A again: same scope -> cache HIT, same content, no upstream call.
			recA2 := postKey(s, body, "A")
			if recA2.Header().Get("X-LLMux-Cache") != "hit" {
				t.Fatal("A repeat should hit its own cache entry")
			}
			if got := contentOf(t, recA2); got != aContent {
				t.Fatalf("A repeat content=%q want %q", got, aContent)
			}

			// Account B, IDENTICAL request: must NOT receive A's cached content. It
			// should miss and get a fresh upstream response.
			recB := postKey(s, body, "B")
			if recB.Code != 200 {
				t.Fatalf("B status=%d body=%s", recB.Code, recB.Body.String())
			}
			if recB.Header().Get("X-LLMux-Cache") == "hit" {
				t.Fatal("CROSS-TENANT LEAK: account B hit account A's cache entry")
			}
			if got := contentOf(t, recB); got == aContent {
				t.Fatalf("CROSS-TENANT LEAK: account B served account A's content %q", got)
			}

			// Upstream was called exactly twice (A's miss + B's miss); A's repeat was
			// served from A's own cache.
			if got := atomic.LoadInt32(&calls); got != 2 {
				t.Fatalf("upstream calls=%d, want 2 (A miss + B miss; A repeat cached)", got)
			}
		})
	}
}

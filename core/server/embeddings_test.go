package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// TestEmbeddingsAllowListEnforced guards the security fix: /v1/embeddings must
// honor the per-key model allow-list (it previously bypassed it).
func TestEmbeddingsAllowListEnforced(t *testing.T) {
	var upstreamHit bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`))
	}))
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
		Keys:      []config.KeyConfig{{Key: "sk", Name: "x", AllowedModels: []string{"allowed-embed"}}},
	}
	s, _ := New(cfg)

	// Forbidden model -> 403, upstream never reached.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"forbidden","input":"x"}`))
	req.Header.Set("Authorization", "Bearer sk")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("forbidden embed model status=%d, want 403", rec.Code)
	}
	if upstreamHit {
		t.Fatal("upstream must not be reached for a disallowed model")
	}

	// Allowed model -> 200.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"allowed-embed","input":"x"}`))
	req.Header.Set("Authorization", "Bearer sk")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("allowed embed model status=%d, want 200", rec.Code)
	}
	var resp openai.EmbeddingResponse
	_ = resp
}

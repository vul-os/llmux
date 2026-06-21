package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

func TestCostAttachedToResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "gpt-4o",
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
			Usage:   &openai.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000, TotalTokens: 2_000_000},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "openai/gpt-4o", Provider: "openai", TargetModel: "gpt-4o"}},
	}
	s, _ := New(cfg)

	rec := post(s, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openai.ChatCompletionResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Usage == nil || resp.Usage.Cost == nil {
		t.Fatalf("expected cost in usage, got %+v", resp.Usage)
	}
	// gpt-4o builtin: 2.5 in + 10 out per MTok => 12.5 for 1M+1M.
	if resp.Usage.Cost.TotalCost != 12.5 {
		t.Fatalf("total cost=%v, want 12.5", resp.Usage.Cost.TotalCost)
	}
}

func TestCatalogEndpoint(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Addr: ":0"}}
	s, _ := New(cfg)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/catalog.json", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var out struct {
		Count  int            `json:"count"`
		Prices map[string]any `json:"prices"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Count == 0 || len(out.Prices) == 0 {
		t.Fatalf("empty catalog export: %s", rec.Body.String())
	}
}

func TestOverrideCostFromConfig(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "gpt-4o",
			Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
			Usage:   &openai.Usage{PromptTokens: 1_000_000, CompletionTokens: 0, TotalTokens: 1_000_000},
		})
	}))
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "openai/gpt-4o", Provider: "openai", TargetModel: "gpt-4o"}},
		Pricing: config.PricingConfig{
			// Pin gpt-4o input to $1/MTok — must beat the built-in $2.5 seed.
			Overrides: map[string]config.PriceOverride{
				"openai/gpt-4o": {InputPerMTok: 1, OutputPerMTok: 1},
			},
		},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	var resp openai.ChatCompletionResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Usage.Cost == nil || resp.Usage.Cost.TotalCost != 1.0 {
		t.Fatalf("override cost not applied: %+v", resp.Usage.Cost)
	}
}

// captureLogger records usage records for assertions (thread-safe for use in
// concurrency tests).
type captureLogger struct {
	mu   sync.Mutex
	recs []UsageRecord
}

func (c *captureLogger) Log(rec UsageRecord) {
	c.mu.Lock()
	c.recs = append(c.recs, rec)
	c.mu.Unlock()
}

func TestUsageLogged(t *testing.T) {
	up := okUpstream("ok", nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, _ := New(cfg)
	cl := &captureLogger{}
	s.usage = cl

	post(s, `{"model":"openai/gpt-4o","messages":[]}`)
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 usage record, got %d", len(cl.recs))
	}
	if cl.recs[0].Model != "openai/gpt-4o" {
		t.Fatalf("logged model=%q", cl.recs[0].Model)
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// upstreamChatOK is a fake provider that counts calls and returns a real usage
// record (1M prompt tokens) — so any UNBILLED call is a visible, real overspend.
func upstreamChatOK(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],` +
			`"usage":{"prompt_tokens":1000000,"completion_tokens":0,"total_tokens":1000000}}`))
	}))
	t.Cleanup(up.Close)
	return up
}

func budgetedChatServer(t *testing.T, budget float64, hits *int32) (*Server, *captureLogger) {
	t.Helper()
	up := upstreamChatOK(t, hits)
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "k"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
		Keys:      []config.KeyConfig{{Key: "sk-budget", Name: "tenant", BudgetUSD: budget}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	cl := &captureLogger{}
	s.usage = cl
	return s, cl
}

func postAuth(s *Server, path, key, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The fail-open this guards: a budgeted key requesting a ROUTABLE but UNPRICED
// model used to be served (real upstream spend) while recordSpend logged $0, so
// the budget never decremented and the key could burn unbounded real spend. It
// must now be refused BEFORE any upstream call.
func TestUnmeterable_BudgetedUnpricedModelRefusedPreflight(t *testing.T) {
	var hits int32
	s, cl := budgetedChatServer(t, 100, &hits)

	// "gpt-unpriced" is not in the (empty) catalog but matches the "*" route.
	rec := postAuth(s, "/v1/chat/completions", "sk-budget", `{"model":"gpt-unpriced","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unpriced model on a budgeted key must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_priced") {
		t.Fatalf("expected model_not_priced error code, got %s", rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("upstream was called %d times — refusal must happen BEFORE any real spend", n)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 0 {
		t.Fatalf("a refused request must record no usage, got %d records", len(cl.recs))
	}
}

// A PRICED model on the same budgeted key is served normally — the guard is
// narrow (it only blocks what genuinely cannot be metered).
func TestUnmeterable_BudgetedPricedModelAllowed(t *testing.T) {
	var hits int32
	s, _ := budgetedChatServer(t, 100, &hits)
	priceModel(t, s, "gpt-priced")

	rec := postAuth(s, "/v1/chat/completions", "sk-budget", `{"model":"gpt-priced","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("priced model must be served, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("priced request must reach upstream exactly once, got %d", n)
	}
}

// An UNLIMITED key (BudgetUSD<=0) is unaffected: there is no budget to evade, so
// an unpriced model is served and logged at $0 as before (no over-blocking).
func TestUnmeterable_UnlimitedKeyUnpricedModelAllowed(t *testing.T) {
	var hits int32
	s, _ := budgetedChatServer(t, 0, &hits)

	rec := postAuth(s, "/v1/chat/completions", "sk-budget", `{"model":"gpt-unpriced","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlimited key must be unaffected by the meterability guard, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("unlimited-key request must reach upstream once, got %d", n)
	}
}

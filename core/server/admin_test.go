package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmux/llmux/core/config"
)

func getAuth(s *Server, path, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	// Present as a loopback caller: keyless /admin + /metrics are loopback-only
	// (fail closed), which models the local/dev operator these helpers stand in
	// for. Non-loopback exposure is covered explicitly by the fail-open regression
	// tests, which set a non-loopback RemoteAddr.
	req.RemoteAddr = "127.0.0.1:12345"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdminRequiresMasterKey(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0", MasterKey: "master"},
		Keys:   []config.KeyConfig{{Key: "sk-a", Name: "team-a", BudgetUSD: 10}},
	}
	s, _ := New(cfg)

	if rec := getAuth(s, "/admin/keys", ""); rec.Code != 401 {
		t.Fatalf("no key: status=%d, want 401", rec.Code)
	}
	if rec := getAuth(s, "/admin/keys", "sk-a"); rec.Code != 401 {
		t.Fatalf("virtual key must not access admin: status=%d, want 401", rec.Code)
	}
	rec := getAuth(s, "/admin/keys", "master")
	if rec.Code != 200 {
		t.Fatalf("master key: status=%d, want 200", rec.Code)
	}
	var out struct {
		Keys []keyStatus `json:"keys"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Keys) != 1 || out.Keys[0].Name != "team-a" {
		t.Fatalf("keys=%+v", out.Keys)
	}
	// Raw key must be masked.
	if out.Keys[0].Key == "sk-a" {
		t.Fatal("raw key must be redacted")
	}
}

func TestAdminUsageAggregates(t *testing.T) {
	up := okUpstream("ok", nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, _ := New(cfg)

	post(s, `{"model":"openai/gpt-4o","messages":[]}`)
	post(s, `{"model":"openai/gpt-4o","messages":[]}`)

	rec := getAuth(s, "/admin/usage", "")
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var out struct {
		Total   Aggregate            `json:"total"`
		ByModel map[string]Aggregate `json:"by_model"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Total.Requests != 2 {
		t.Fatalf("total requests=%d, want 2", out.Total.Requests)
	}
	if out.ByModel["openai/gpt-4o"].Requests != 2 {
		t.Fatalf("by_model=%+v", out.ByModel)
	}
	if out.Total.CostUSD <= 0 {
		t.Fatalf("expected cost aggregated, got %v", out.Total.CostUSD)
	}
}

func TestAdminKeysShowsSpend(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "gpt-4o",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1000000, "completion_tokens": 0, "total_tokens": 1000000},
		})
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "m"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "openai/gpt-4o", Provider: "openai", TargetModel: "gpt-4o"}},
		Keys:      []config.KeyConfig{{Key: "sk-a", Name: "team-a", BudgetUSD: 100}},
	}
	s, _ := New(cfg)

	// One priced request charges the key (gpt-4o input = $2.5/MTok * 1M = $2.5).
	postKey(s, `{"model":"openai/gpt-4o","messages":[]}`, "sk-a")

	rec := getAuth(s, "/admin/keys", "m")
	var out struct {
		Keys []keyStatus `json:"keys"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Keys) != 1 || out.Keys[0].SpendUSD != 2.5 {
		t.Fatalf("spend not reflected: %+v", out.Keys)
	}
}

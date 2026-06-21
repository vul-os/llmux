package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

func TestMetricsRequiresMasterKey(t *testing.T) {
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0", MasterKey: "m"}})
	if rec := getAuth(s, "/metrics", ""); rec.Code != 401 {
		t.Fatalf("/metrics without key=%d, want 401", rec.Code)
	}
	if rec := getAuth(s, "/metrics", "m"); rec.Code != 200 {
		t.Fatalf("/metrics with master=%d, want 200", rec.Code)
	}
}

func TestHealthDisclosureGated(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "m"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: "https://x/v1"}},
	}
	s, _ := New(cfg)
	// Unauthenticated: status only, no provider/topology disclosure.
	rec := getAuth(s, "/health", "")
	if strings.Contains(rec.Body.String(), "providers") {
		t.Fatalf("unauth /health leaked providers: %s", rec.Body.String())
	}
	// With master key: full disclosure.
	rec = getAuth(s, "/health", "m")
	if !strings.Contains(rec.Body.String(), "providers") {
		t.Fatalf("admin /health missing providers: %s", rec.Body.String())
	}
}

func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	// Point at an unreachable upstream; the dial error contains the URL/host,
	// which must NOT be echoed to the client.
	badURL := "http://127.0.0.1:1/v1"
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: badURL}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code != 502 {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "127.0.0.1") || strings.Contains(body, "dial") {
		t.Fatalf("transport error leaked internal details: %s", body)
	}
	var er openai.ErrorResponse
	json.Unmarshal([]byte(body), &er)
	if er.Error.Message != "upstream request failed" {
		t.Fatalf("message=%q, want generic", er.Error.Message)
	}
}

func TestCacheScopedPerKey(t *testing.T) {
	// Two keys sending the identical body must NOT share cached responses.
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "m",
			Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Cache:     config.CacheConfig{Enabled: true, MaxEntries: 100},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
		Keys:      []config.KeyConfig{{Key: "sk-a", Name: "a"}, {Key: "sk-b", Name: "b"}},
	}
	s, _ := New(cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"same"}]}`
	postKey(s, body, "sk-a") // miss -> upstream (calls=1)
	postKey(s, body, "sk-a") // hit for a (calls stays 1)
	postKey(s, body, "sk-b") // different key -> must miss -> upstream (calls=2)
	if calls != 2 {
		t.Fatalf("upstream calls=%d, want 2 (keys must not share cache)", calls)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

func TestRateLimitHeaderRelayed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit-Requests", "100")
		w.Header().Set("X-RateLimit-Remaining-Requests", "99")
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "m",
			Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Header().Get("X-RateLimit-Limit-Requests") != "100" {
		t.Fatalf("rate-limit header not relayed: %v", rec.Header())
	}
}

func TestUpstreamErrorTaxonomy(t *testing.T) {
	cases := []struct {
		status int
		typ    string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "server_error"},
	}
	for _, c := range cases {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			json.NewEncoder(w).Encode(openai.NewError("boom", c.typ, ""))
		}))
		cfg := &config.Config{
			Server:    config.ServerConfig{Addr: ":0"},
			Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
			Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
			Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
		}
		s, _ := New(cfg)
		rec := post(s, `{"model":"m","messages":[]}`)
		if rec.Code != c.status {
			t.Errorf("status=%d, want %d (faithful relay)", rec.Code, c.status)
		}
		var er openai.ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &er)
		if er.Error.Type != c.typ {
			t.Errorf("type=%q, want %q", er.Error.Type, c.typ)
		}
		up.Close()
	}
}

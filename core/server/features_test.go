package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// okUpstream returns a 200 chat completion echoing a label in the content.
func okUpstream(label string, counter *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if counter != nil {
			atomic.AddInt32(counter, 1)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		resp := openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: body["model"].(string),
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str(label)}, FinishReason: "stop"}},
			Usage:   &openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

// flakyUpstream fails with status `fail` for the first `failures` calls, then 200.
func flakyUpstream(failures int, fail int, counter *int32) *httptest.Server {
	var n int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(counter, 1)
		if atomic.AddInt32(&n, 1) <= int32(failures) {
			w.WriteHeader(fail)
			json.NewEncoder(w).Encode(openai.NewError("temporary", "server_error", ""))
			return
		}
		resp := openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "m",
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str("recovered")}, FinishReason: "stop"}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func post(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func postKey(s *Server, body, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestFailover(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(openai.NewError("down", "server_error", ""))
	}))
	defer bad.Close()
	good := okUpstream("from-good", nil)
	defer good.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Retry:  config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{
			{Name: "bad", Type: config.TypePassthrough, BaseURL: bad.URL + "/v1"},
			{Name: "good", Type: config.TypePassthrough, BaseURL: good.URL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "m", Provider: "bad", Fallbacks: []string{"good"}}},
	}
	s, _ := New(cfg)

	rec := post(s, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openai.ChatCompletionResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Choices[0].Message.Content.String() != "from-good" {
		t.Fatalf("content=%q (expected failover to good)", resp.Choices[0].Message.Content.String())
	}
}

func TestNoFailoverOnClientError(t *testing.T) {
	var goodCalls int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(openai.NewError("bad request", "invalid_request_error", ""))
	}))
	defer bad.Close()
	good := okUpstream("from-good", &goodCalls)
	defer good.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 2, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "bad", Type: config.TypePassthrough, BaseURL: bad.URL + "/v1"}, {Name: "good", Type: config.TypePassthrough, BaseURL: good.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "m", Provider: "bad", Fallbacks: []string{"good"}}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code != 400 {
		t.Fatalf("status=%d, want 400 (no failover on client error)", rec.Code)
	}
	if atomic.LoadInt32(&goodCalls) != 0 {
		t.Fatalf("good was called %d times; should not fail over on 400", goodCalls)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	var calls int32
	up := flakyUpstream(1, 503, &calls)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 2, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "m", Provider: "p"}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls=%d, want 2 (1 fail + 1 retry)", calls)
	}
}

func TestExactCache(t *testing.T) {
	var calls int32
	up := okUpstream("cached", &calls)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Cache:     config.CacheConfig{Enabled: true, MaxEntries: 100},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "m", Provider: "p"}},
	}
	s, _ := New(cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	rec1 := post(s, body)
	if rec1.Code != 200 || rec1.Header().Get("X-LLMux-Cache") == "hit" {
		t.Fatalf("first call should miss: %d %s", rec1.Code, rec1.Header().Get("X-LLMux-Cache"))
	}
	rec2 := post(s, body)
	if rec2.Header().Get("X-LLMux-Cache") != "hit" {
		t.Fatalf("second call should hit cache")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("upstream called %d times, want 1 (cache served second)", calls)
	}
}

func TestVirtualKeyAuthAndRateLimit(t *testing.T) {
	up := okUpstream("ok", nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "m", Provider: "p"}},
		Keys: []config.KeyConfig{
			{Key: "sk-good", Name: "team", RPM: 1, AllowedModels: []string{"m"}},
		},
	}
	s, _ := New(cfg)
	body := `{"model":"m","messages":[]}`

	if rec := postKey(s, body, "sk-bad"); rec.Code != 401 {
		t.Fatalf("bad key status=%d, want 401", rec.Code)
	}
	if rec := postKey(s, body, "sk-good"); rec.Code != 200 {
		t.Fatalf("good key first status=%d, want 200", rec.Code)
	}
	if rec := postKey(s, body, "sk-good"); rec.Code != 429 {
		t.Fatalf("good key second status=%d, want 429 (rpm=1)", rec.Code)
	}
}

func TestModelAllowList(t *testing.T) {
	up := okUpstream("ok", nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
		Keys:      []config.KeyConfig{{Key: "sk", Name: "x", AllowedModels: []string{"allowed"}}},
	}
	s, _ := New(cfg)
	if rec := postKey(s, `{"model":"forbidden","messages":[]}`, "sk"); rec.Code != 403 {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	if rec := postKey(s, `{"model":"allowed","messages":[]}`, "sk"); rec.Code != 200 {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
}

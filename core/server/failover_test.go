package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// streamUpstream is a mock that streams two SSE chunks then [DONE].
func streamUpstream(label string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, tok := range []string{label, "!"} {
			c := openai.ChatCompletionChunk{Object: "chat.completion.chunk", Choices: []openai.ChunkChoice{{Delta: openai.Delta{Content: tok}}}}
			b, _ := json.Marshal(c)
			w.Write([]byte("data: " + string(b) + "\n\n"))
			fl.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
}

func down(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(openai.NewError("down", "server_error", ""))
	}))
}

func TestStreamFailoverBeforeFirstChunk(t *testing.T) {
	bad := down(503)
	defer bad.Close()
	good := streamUpstream("OK")
	defer good.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"}, Retry: config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{
			{Name: "bad", Type: config.TypePassthrough, BaseURL: bad.URL + "/v1"},
			{Name: "good", Type: config.TypePassthrough, BaseURL: good.URL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "m", Provider: "bad", Fallbacks: []string{"good"}}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[],"stream":true}`)
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected SSE after failover, got %q (%d)", rec.Header().Get("Content-Type"), rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "OK") || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("failover stream content missing: %s", rec.Body.String())
	}
}

func TestStreamAllFailBeforeChunkIsJSONError(t *testing.T) {
	b1, b2 := down(503), down(503)
	defer b1.Close()
	defer b2.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"}, Retry: config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{
			{Name: "a", Type: config.TypePassthrough, BaseURL: b1.URL + "/v1"},
			{Name: "b", Type: config.TypePassthrough, BaseURL: b2.URL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "m", Provider: "a", Fallbacks: []string{"b"}}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[],"stream":true}`)
	// No chunk ever emitted -> a normal JSON error, not a partial 200 SSE.
	if rec.Code != 503 {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
		t.Fatal("should not be an SSE stream when nothing streamed")
	}
}

func TestFailoverChainThreeProviders(t *testing.T) {
	var c3 int32
	b1, b2 := down(503), down(503)
	defer b1.Close()
	defer b2.Close()
	good := okUpstream("from-p3", &c3)
	defer good.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"}, Retry: config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{
			{Name: "p1", Type: config.TypePassthrough, BaseURL: b1.URL + "/v1"},
			{Name: "p2", Type: config.TypePassthrough, BaseURL: b2.URL + "/v1"},
			{Name: "p3", Type: config.TypePassthrough, BaseURL: good.URL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "m", Provider: "p1", Fallbacks: []string{"p2", "p3"}}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openai.ChatCompletionResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Choices[0].Message.Content.String() != "from-p3" {
		t.Fatalf("content=%q, want from-p3", resp.Choices[0].Message.Content.String())
	}
	if atomic.LoadInt32(&c3) != 1 {
		t.Fatalf("p3 calls=%d, want 1", c3)
	}
}

func TestRetryExhaustionRelaysLastError(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
		json.NewEncoder(w).Encode(openai.NewError("always down", "server_error", ""))
	}))
	defer up.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"}, Retry: config.RetryConfig{MaxRetries: 2, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "m", Provider: "p"}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code != 503 {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls=%d, want 3 (1 + 2 retries)", got)
	}
}

func TestRateLimitHeaderRelayedOnError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining-Requests", "0")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(openai.NewError("slow down", "rate_limit_error", ""))
	}))
	defer up.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"}, Retry: config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, _ := New(cfg)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code != 429 {
		t.Fatalf("status=%d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "30" || rec.Header().Get("X-RateLimit-Remaining-Requests") != "0" {
		t.Fatalf("rate-limit headers not relayed on error: %v", rec.Header())
	}
}

func TestConcurrentChatRequestsRace(t *testing.T) {
	up := okUpstream("ok", nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Cache:     config.CacheConfig{Enabled: true, MaxEntries: 100},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, _ := New(cfg)
	cl := &captureLogger{}
	s.usage = cl
	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`
			rec := post(s, body)
			if rec.Code != 200 {
				t.Errorf("status=%d", rec.Code)
			}
		}(i)
	}
	wg.Wait()
	if got := s.stats.snapshot()["total"].(Aggregate).Requests; got != N {
		t.Fatalf("usage total requests=%d, want %d", got, N)
	}
}

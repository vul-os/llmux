package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
)

func TestModalityForwardJSON(t *testing.T) {
	var gotPath, gotModel, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"img-1","data":[{"url":"http://x/y.png"}]}`))
	}))
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "k"}},
		Routes:    []config.RouteConfig{{Model: "dall-e-3", Provider: "openai", TargetModel: "dall-e-3-real"}},
	}
	s, _ := New(cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"dall-e-3","prompt":"a cat"}`))
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("upstream path=%q", gotPath)
	}
	if gotModel != "dall-e-3-real" {
		t.Fatalf("model not rewritten to target: %q", gotModel)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("provider auth not set: %q", gotAuth)
	}
	if !strings.Contains(rec.Body.String(), "y.png") {
		t.Fatalf("response not relayed: %s", rec.Body.String())
	}
}

func TestModalityMissingModel(t *testing.T) {
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/moderations", strings.NewReader(`{"input":"x"}`)))
	if rec.Code != 400 {
		t.Fatalf("status=%d, want 400 (missing model)", rec.Code)
	}
}

func TestModalityUnsupportedByAdapter(t *testing.T) {
	// Anthropic is not a Forwarder -> modality endpoint returns 501.
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "anthropic", Type: config.TypeAnthropic, APIKey: "k"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "anthropic"}},
	}
	s, _ := New(cfg)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/rerank", strings.NewReader(`{"model":"x","query":"q","documents":["a"]}`)))
	if rec.Code != 501 {
		t.Fatalf("status=%d, want 501 (adapter has no Forwarder)", rec.Code)
	}
}

func TestModalityStreamingForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		w.Write([]byte("data: {\"text\":\"hel\"}\n\n"))
		fl.Flush()
		w.Write([]byte("data: {\"text\":\"lo\"}\n\n"))
		fl.Flush()
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "k"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, _ := New(cfg)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/completions", strings.NewReader(`{"model":"m","prompt":"x","stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("stream not relayed: %s", rec.Body.String())
	}
}

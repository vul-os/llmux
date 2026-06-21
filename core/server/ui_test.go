package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
)

func TestUIServed(t *testing.T) {
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})

	// /ui -> redirect to /ui/
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	if rec.Code != 301 {
		t.Fatalf("/ui status=%d, want 301", rec.Code)
	}

	// /ui/ -> index.html
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("/ui/ did not serve the app: %d", rec.Code)
	}

	// SPA fallback: unknown client route returns index.html, not 404.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/app", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("SPA fallback failed: %d", rec.Code)
	}
}

func TestUIPublicWithMasterKey(t *testing.T) {
	// Even with a master key set, /ui assets must be reachable without it
	// (the dashboard authenticates client-side).
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0", MasterKey: "m"}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 {
		t.Fatalf("/ui/ should be public, got %d", rec.Code)
	}
}

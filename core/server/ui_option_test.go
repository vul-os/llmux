package server

import (
	"net/http/httptest"
	"testing"

	"github.com/vul-os/llmux/core/config"
)

// Options.UI defaults to true so `llmux serve` is unchanged, and setting it
// false removes the console route entirely (an API-only gateway). This holds
// in both build-tag states, so it is not tagged.
func TestUIOptionDefaultsOn(t *testing.T) {
	if !DefaultOptions().UI {
		t.Fatal("DefaultOptions().UI must be true — `llmux serve` keeps the console")
	}
	s, err := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code == 404 {
		t.Fatal("/ui/ 404s by default — the console route was not mounted")
	}
}

func TestUIOptionOffRemovesRoute(t *testing.T) {
	s, err := NewWithOptions(&config.Config{Server: config.ServerConfig{Addr: ":0"}}, Options{UI: false})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	for _, path := range []string{"/ui", "/ui/", "/ui/licenses.txt"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Errorf("%s status=%d with UI:false, want 404 (route must not exist)", path, rec.Code)
		}
	}
	// The API is unaffected.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("/v1/models status=%d with UI:false, want 200", rec.Code)
	}
}

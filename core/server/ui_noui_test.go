//go:build noui

package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/vul-os/llmux/core/config"
)

// Under the `noui` build tag the admin console is not compiled in. /ui must
// then say so in machine-readable form rather than 404 (which reads as "wrong
// URL") or serve a blank page — and the JSON API must be unaffected.
func TestUIStubUnderNoUITag(t *testing.T) {
	s, err := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 501 {
		t.Fatalf("/ui/ status=%d, want 501 (console not built in)", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/ui/ stub is not JSON: %v — %q", err, rec.Body.String())
	}
	if body.Error.Code != "ui_not_built" {
		t.Fatalf("/ui/ stub code=%q, want ui_not_built (body=%s)", body.Error.Code, rec.Body.String())
	}

	// The API is untouched by dropping the console.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("/v1/models status=%d under noui, want 200", rec.Code)
	}
}

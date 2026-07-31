package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// This file covers, in Go, what web/e2e/boot.e2e.js and security.e2e.js used
// to prove in a real browser before the admin UI became a single hand-written
// HTML file with no JS toolchain to run them:
//
//   - the UI is served at /ui, with a redirect from the bare path (boot.e2e.js)
//   - it is public (no master key required to load the page itself), even
//     when a master key IS configured for /admin (security.e2e.js's
//     credential-hygiene tests assume this: the page must load so the master
//     key can be entered in the first place)
//   - there is no dangling reference: unlike the old SPA-fallback bundle,
//     there is exactly one page and nothing else lives under /ui/, so an
//     unknown path 404s instead of silently serving the dashboard
//   - the security headers appropriate for a page holding the admin master
//     key are present
//   - /admin/* still requires the master key regardless of /ui being public
//
// What this file does NOT cover: in-browser behaviour (does clicking a tab
// actually switch views, does a hostile model id render as inert text, is
// the master key ever written to the console). That needs a real JS runtime;
// web/embed_test.go asserts the static properties a Go test CAN check (the
// required elements are present, no external origin is referenced), and the
// rest was verified once, manually, in a real browser — see the web rewrite
// task notes. There is no repeatable substitute for a DOM-executing test
// without reintroducing the toolchain this rewrite removes.

func TestUIServed(t *testing.T) {
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})

	// /ui -> redirect to /ui/
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui", nil))
	if rec.Code != 301 {
		t.Fatalf("/ui status=%d, want 301", rec.Code)
	}

	// /ui/ -> the admin console.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 {
		t.Fatalf("/ui/ status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-tab="usage"`, `data-tab="keys"`, `data-tab="models"`, "llmux_master_key"} {
		if !strings.Contains(body, want) {
			t.Errorf("/ui/ response missing %q — did not serve the admin console", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("/ui/ Content-Type=%q, want text/html; charset=utf-8", ct)
	}
}

func TestUIUnknownPathNotFound(t *testing.T) {
	// The old bundle had a client-side SPA fallback (unknown /ui/* paths
	// served index.html so client routes survived a refresh). The console is
	// now one page with no client-side routes beyond a URL fragment, which
	// never reaches the server — so there is nothing left to fall back to,
	// and an unknown path under /ui/ should 404 rather than silently serving
	// the dashboard for a typo'd or stale asset URL.
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/app", nil))
	if rec.Code != 404 {
		t.Fatalf("/ui/app status=%d, want 404 (no such asset exists in a single-page UI)", rec.Code)
	}
}

func TestUIPublicWithMasterKey(t *testing.T) {
	// Even with a master key set, /ui must be reachable without it (the
	// dashboard authenticates to /admin client-side, so the page itself has
	// to load before a key can ever be entered).
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0", MasterKey: "m"}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 {
		t.Fatalf("/ui/ should be public, got %d", rec.Code)
	}
}

func TestUILicensesTxtServed(t *testing.T) {
	// Go-dependency attribution must stay reachable from a running binary
	// (not just visible in the repo) and, like the rest of /ui, without a
	// master key.
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0", MasterKey: "m"}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/licenses.txt", nil))
	if rec.Code != 200 {
		t.Fatalf("/ui/licenses.txt status=%d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("/ui/licenses.txt Content-Type=%q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "THIRD-PARTY NOTICES") {
		t.Errorf("/ui/licenses.txt did not serve the notices file")
	}
}

func TestUISecurityHeaders(t *testing.T) {
	// The console holds the admin master key. Defense in depth beyond "React
	// escapes text children" (which no longer applies — there is no React):
	// no inline-script injection can be smuggled in from another origin, the
	// page cannot be framed, and content sniffing is disabled.
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0"}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options=%q, want nosniff", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options=%q, want DENY", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy header missing on /ui/")
	}
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy missing %q: %q", want, csp)
		}
	}
}

func TestUIAdminStillRequiresMasterKeyRegardlessOfUIBeingPublic(t *testing.T) {
	// /ui being public must not have widened /admin. A non-loopback caller
	// with no key must still be refused.
	s, _ := New(&config.Config{Server: config.ServerConfig{Addr: ":0", MasterKey: "m"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("/admin/keys without a key status=%d, want 401", rec.Code)
	}
}

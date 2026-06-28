package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// ---------------------------------------------------------------------------
// SSRF via HTTP redirect prevention (host allowlist / dial-pin)
// ---------------------------------------------------------------------------

// TestSSRFProviderRedirectNotFollowed verifies that a provider endpoint that
// returns a 3xx redirect to another host does NOT cause the gateway to follow
// that redirect. This is the primary mitigation for redirect-based SSRF, where
// a compromised or malicious upstream could redirect a POST to an internal
// metadata endpoint (e.g. http://169.254.169.254/).
//
// The provider's HTTP clients have CheckRedirect=noRedirect so redirects are
// treated as upstream errors, not silently followed.
func TestSSRFProviderRedirectNotFollowed(t *testing.T) {
	// Internal endpoint that should NEVER be reached via a redirect.
	var internalHit bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHit = true
		w.WriteHeader(200)
	}))
	defer internal.Close()

	// "Provider" endpoint that responds with a 302 redirect to the internal server.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+r.URL.Path, http.StatusFound)
	}))
	defer redirecting.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: redirecting.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := post(s, chatBody("m"))

	// The redirect must NOT be followed: the internal endpoint must never be hit.
	if internalHit {
		t.Fatal("SSRF: redirect was followed to internal endpoint — CheckRedirect not enforced")
	}

	// The response to the client should be an error (not a 200 from the internal
	// server), since the 3xx is treated as an upstream failure.
	if rec.Code == 200 {
		t.Fatalf("redirect returned 200 — gateway may have followed it: body=%s", rec.Body.String())
	}
}

// TestSSRFPermanentRedirectNotFollowed is the same guard for 301 Moved Permanently.
func TestSSRFPermanentRedirectNotFollowed(t *testing.T) {
	var internalHit bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHit = true
		w.WriteHeader(200)
	}))
	defer internal.Close()

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer redirecting.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: redirecting.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := post(s, chatBody("m"))

	if internalHit {
		t.Fatal("SSRF: 301 redirect was followed to internal endpoint — CheckRedirect not enforced")
	}
	if rec.Code == 200 {
		t.Fatalf("301 redirect returned 200 — body=%s", rec.Body.String())
	}
}

// TestSSRFModelNameCannotRedirectOutboundHost verifies that a model name
// containing a URL-like string (e.g., "http://169.254.169.254/...") is
// rejected at the routing layer and never causes an outbound connection to the
// embedded URL. This is distinct from redirect SSRF — it is about ensuring the
// router treats model names as opaque strings, not destinations.
func TestSSRFModelNameCannotRedirectOutboundHost(t *testing.T) {
	var outboundHit bool
	// A real server at "the injected URL": if the gateway were to make an outbound
	// call to this URL directly, outboundHit would be set.
	injected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outboundHit = true
		w.WriteHeader(200)
	}))
	defer injected.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: "http://127.0.0.1:1/v1"}},
		// No catch-all route: only named providers can be targeted.
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Model name that looks like the URL of the injected server.
	rec := post(s, chatBody(injected.URL+"/v1/chat/completions"))

	// Must not reach the injected server.
	if outboundHit {
		t.Fatal("SSRF: gateway made an outbound call to the URL embedded in the model name")
	}
	// Must not be a 200 (would imply successful routing).
	if rec.Code == 200 {
		t.Fatalf("URL-shaped model returned 200 — routing vulnerability: body=%s", rec.Body.String())
	}
}

// TestSSRFProviderResponseBodyNotEchoedOnRedirect verifies that when a
// provider endpoint redirects (and the redirect is blocked), the redirect
// Location header or target URL is never echoed verbatim in the client-visible
// error response. Leaking internal redirect targets could aid reconnaissance.
func TestSSRFProviderResponseBodyNotEchoedOnRedirect(t *testing.T) {
	const internalTarget = "http://169.254.169.254/latest/meta-data/"
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", internalTarget)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirecting.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: redirecting.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := post(s, chatBody("m"))
	body := rec.Body.String()
	if containsAny(body, []string{"169.254", "meta-data", internalTarget}) {
		t.Fatalf("redirect target leaked in client-visible response: %s", body)
	}

	// The Location header must not be forwarded to the client either.
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("redirect Location header forwarded to client: %q", loc)
	}
}

// TestSSRFConfiguredProviderOnlyCallsConfiguredBaseURL verifies that a
// configured passthrough provider only makes outbound calls to ITS configured
// base URL. The provider may not be "re-pointed" by any request-time input
// such as model names or message content.
func TestSSRFConfiguredProviderOnlyCallsConfiguredBaseURL(t *testing.T) {
	var legitimateHit bool
	legitimate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legitimateHit = true
		json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer legitimate.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: legitimate.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Normal request: hits the legitimate upstream.
	if rec := post(s, chatBody("m")); rec.Code != 200 {
		t.Fatalf("normal request: status=%d, want 200", rec.Code)
	}
	if !legitimateHit {
		t.Fatal("legitimate upstream was not called")
	}

	// A request with an injected URL in the model name must NOT hit the injected
	// URL — the model name is forwarded as a JSON field, not as an outbound URL.
	legitimateHit = false
	var spoofHit bool
	spoof := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spoofHit = true
		w.WriteHeader(200)
	}))
	defer spoof.Close()

	// Model name containing a URL — if the gateway ever uses model names as URLs,
	// spoofHit would be set.
	rec := post(s, chatBody(spoof.URL+"/v1/chat/completions"))
	if spoofHit {
		t.Fatal("SSRF: outbound call made to URL embedded in model name")
	}
	_ = rec
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if len(n) > 0 {
			idx := 0
			for idx < len(s)-len(n)+1 {
				if s[idx:idx+len(n)] == n {
					return true
				}
				idx++
			}
		}
	}
	return false
}

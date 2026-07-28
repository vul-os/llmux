package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// llmux is configured by ENDPOINT, not by account: a consumer points an OpenAI
// client at a URL and presents an operator-issued bearer token. There is no
// login, no signup, no email, no session, no cookie — and a consumer writing a
// client is entitled to rely on that. These tests keep it true.

// loginlessServer builds a gateway with a master key and one virtual key, i.e.
// the fully-credentialed posture, so a 404 below means "no such surface"
// rather than "unauthenticated".
func loginlessServer(t *testing.T) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: "127.0.0.1:0", MasterKey: "master"},
		Providers: []config.ProviderConfig{{Name: "local", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "local"}},
		Keys:      []config.KeyConfig{{Key: "sk-test", Name: "test"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestNoLoginEndpoints proves the gateway serves no authentication surface of
// its own. Every path a consumer might expect from an account-based product
// must simply not exist.
func TestNoLoginEndpoints(t *testing.T) {
	s := loginlessServer(t)
	h := s.Handler()

	paths := []string{
		"/login", "/logout", "/signup", "/register", "/session", "/sessions",
		"/auth", "/auth/login", "/auth/callback", "/auth/session",
		"/oauth/authorize", "/oauth/token", "/api/auth/session",
		"/v1/login", "/v1/auth", "/admin/login", "/password/reset", "/verify-email",
	}
	// Probe with both credentials the gateway knows about. A login/session
	// endpoint would answer 2xx (a token/redirect body) or 3xx (an IdP bounce);
	// anything >= 400 means the surface simply is not there. Authentication runs
	// before routing, so an unknown path answers 401 or 404 depending on which
	// credential was presented — both are "no login here".
	for _, p := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, token := range []string{"master", "sk-test"} {
				req := httptest.NewRequest(method, p, nil)
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code < 400 {
					t.Errorf("LOGIN SURFACE: %s %s (as %s) returned %d — llmux authenticates with an "+
						"operator-issued bearer token only; it must have no account or login surface",
						method, p, token, w.Code)
				}
			}
		}
	}
	if len(paths) < 10 {
		t.Fatal("login-surface probe list shrank below a useful size; this test would verify little")
	}
}

// TestNoCookiesIssued proves the gateway never sets a cookie: there is no
// session to carry, so a client needs no cookie jar and no CSRF handling.
func TestNoCookiesIssued(t *testing.T) {
	s := loginlessServer(t)
	h := s.Handler()

	checked := 0
	for _, tc := range []struct{ method, path, auth string }{
		{http.MethodGet, "/health", ""},
		{http.MethodGet, "/health", "master"},
		{http.MethodGet, "/v1/models", "sk-test"},
		{http.MethodGet, "/admin/keys", "master"},
		{http.MethodGet, "/admin/usage", "master"},
		{http.MethodGet, "/metrics", "master"},
		{http.MethodGet, "/ui/", ""},
		{http.MethodGet, "/v1/catalog.json", "sk-test"},
		{http.MethodPost, "/v1/chat/completions", "bad-token"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		if tc.auth != "" {
			req.Header.Set("Authorization", "Bearer "+tc.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		checked++
		if got := w.Result().Header.Values("Set-Cookie"); len(got) > 0 {
			t.Errorf("SESSION SURFACE: %s %s set a cookie (%v) — llmux is stateless bearer-token auth",
				tc.method, tc.path, got)
		}
	}
	if checked == 0 {
		t.Fatal("no responses were checked for cookies; this test verified nothing")
	}
}

// TestServerReadsNoCredentialButBearer is the structural half: the gateway's
// source must not touch cookies at all. An HTTP probe can only test the paths
// it thinks to ask for; this catches a session mechanism the moment it is
// written, on any route.
func TestServerReadsNoCredentialButBearer(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}

	// Selectors that would mean session/cookie credentials.
	banned := map[string]string{
		"SetCookie": "http.SetCookie",
		"Cookie":    "Request.Cookie / http.Cookie",
		"Cookies":   "Request.Cookies",
	}

	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if what, bad := banned[sel.Sel.Name]; bad {
				t.Errorf("SESSION SURFACE: %s:%d references %s — llmux authenticates with an "+
					"Authorization bearer header only; cookies would introduce a session/login model "+
					"that consumers and docs (docs/api.md) say does not exist",
					name, fset.Position(sel.Pos()).Line, what)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("scanned no core/server source files; this test verified nothing")
	}
	t.Logf("no-login guard: %d core/server files scanned for cookie/session credentials", checked)
}

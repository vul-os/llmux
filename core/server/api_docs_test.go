package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// llmux is consumed by ENDPOINT: another product is handed a URL and a bearer
// token and is expected to write a client against docs/api.md without reading
// this package. That only holds if the doc is complete, so these tests check it
// in both directions — every served route is documented, and every documented
// route is served.

// apiDocPath is the reference a consumer is pointed at.
const apiDocPath = "../../docs/api.md"

// routeLiteralRE matches a Go ServeMux pattern literal ("POST /v1/embeddings").
var routeLiteralRE = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD) (/\S*)$`)

// docRouteRE matches a route as docs/api.md writes it: `POST /v1/embeddings`.
var docRouteRE = regexp.MustCompile("`(GET|POST|PUT|DELETE|PATCH|HEAD) (/[^`]*)`")

// route is one method+path pair.
type route struct{ method, path string }

func (r route) String() string { return r.method + " " + r.path }

// normalizePath trims a trailing slash so a mux prefix pattern ("GET /ui/")
// compares equal to how the docs name it ("GET /ui").
func normalizePath(p string) string {
	if len(p) > 1 {
		return strings.TrimRight(p, "/")
	}
	return p
}

// servedRoutes reads every ServeMux pattern registered anywhere in this package
// out of the source. Patterns come from HandleFunc calls and from the
// modalityRoutes / transcriptionRoutes tables, all of which are plain string
// literals, so the census cannot fall behind the code.
func servedRoutes(t *testing.T) []route {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	seen := map[route]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v := strings.Trim(lit.Value, `"`)
			if m := routeLiteralRE.FindStringSubmatch(v); m != nil {
				seen[route{m[1], normalizePath(m[2])}] = true
			}
			return true
		})
	}
	// /metrics is served by metricsMW rather than the mux, so it has no pattern
	// literal to find. It is a documented surface, so include it explicitly.
	seen[route{http.MethodGet, "/metrics"}] = true

	out := make([]route, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// minRoutes is a coverage floor so a broken scan fails instead of passing by
// finding nothing (chat, embeddings, models, catalog, health, metrics, 6
// modality routes, 2 audio-input routes, 2 admin, 3 BYOK, 2 UI).
const minRoutes = 20

// TestAPIDocsCoverEveryServedRoute fails when a route is added without being
// documented — the failure a consumer would otherwise hit as an undiscoverable
// endpoint.
func TestAPIDocsCoverEveryServedRoute(t *testing.T) {
	doc, err := os.ReadFile(apiDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", apiDocPath, err)
	}
	text := string(doc)

	routes := servedRoutes(t)
	if len(routes) < minRoutes {
		t.Fatalf("found only %d routes (floor %d) — the route scan is broken and this test verified nothing: %v",
			len(routes), minRoutes, routes)
	}
	for _, r := range routes {
		if !strings.Contains(text, r.method+" "+r.path) {
			t.Errorf("UNDOCUMENTED ROUTE: %s is served but does not appear in docs/api.md. A consumer is "+
				"expected to write a client from that page without reading this package, so every route "+
				"must be listed there with its auth and semantics.", r)
		}
	}
	t.Logf("api docs: %d served routes checked against docs/api.md", len(routes))
}

// TestAPIDocsDocumentNoPhantomRoutes is the other direction: a route named in
// docs/api.md must actually answer. Documenting an endpoint that 404s sends a
// consumer to write a client against something that does not exist.
func TestAPIDocsDocumentNoPhantomRoutes(t *testing.T) {
	doc, err := os.ReadFile(apiDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", apiDocPath, err)
	}
	s := loginlessServer(t)
	h := s.Handler()

	// The /ui routes are served from the go:embed'ed web bundle; whether they
	// answer depends on a Node build having run, which is not this package's
	// contract. Exclude them EXPLICITLY (and say so) rather than silently.
	skipped := []string{}
	checked := 0
	for _, m := range docRouteRE.FindAllStringSubmatch(string(doc), -1) {
		method, path := m[1], m[2]
		if strings.HasPrefix(path, "/ui") {
			skipped = append(skipped, method+" "+path)
			continue
		}
		// Substitute a concrete value for wildcard segments (/admin/byok/{account}).
		concrete := regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(path, "x")

		// Present the credential the doc says this surface takes, so a 401 can
		// never be mistaken for "the route exists". Authentication runs before
		// routing, so the wrong token would mask a missing route with a 401.
		token := "sk-test"
		if strings.HasPrefix(path, "/admin") || strings.HasPrefix(path, "/metrics") {
			token = "master"
		}
		req := httptest.NewRequest(method, concrete, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		checked++
		switch w.Code {
		case http.StatusNotFound:
			t.Errorf("PHANTOM ROUTE: docs/api.md documents %s %s but the gateway returns 404 for it",
				method, path)
		case http.StatusUnauthorized:
			t.Errorf("AUTH MISMATCH: docs/api.md documents %s %s, but the credential the page says it takes "+
				"(%s) is rejected — a consumer following the doc would get 401", method, path, token)
		}
	}
	if checked < minRoutes-len(skipped) {
		t.Fatalf("only %d documented routes were probed (skipped %v) — the doc scan is broken and this test "+
			"verified almost nothing", checked, skipped)
	}
	if len(skipped) > 0 {
		t.Logf("api docs: NOT VERIFIED (depends on the embedded web build, not on this package): %v",
			strings.Join(skipped, ", "))
	}
	t.Logf("api docs: %d documented routes probed against the live handler", checked)
}

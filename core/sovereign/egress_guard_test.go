package sovereign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file is the STRUCTURAL half of the sovereignty guarantee. The behavioral
// half lives in core/server (sovereignty_test.go, sovereignty_enforcement_test.go)
// and proves that a blocked provider never opens a socket on the dispatch paths
// that exist TODAY. That is enforcement by test coverage, which decays the
// moment someone adds a seventh dispatch path.
//
// The tests here read the SOURCE instead of running it, so a NEW egress path
// fails CI the day it is written:
//
//   - TestNoUngatedProviderDispatch  — every call to a provider's egress-capable
//     method must sit in a function that also calls enforceSovereignty.
//   - TestOutboundDialSitesAreRegistered — every file in the repo that can open
//     an outbound connection must be listed in outboundDialSites below, with a
//     reason stating whether the sovereignty gate covers it. A new dialer is a
//     test failure until a human writes that sentence.
//   - TestServerPackageNeverDialsDirectly — the gateway's own handlers may only
//     reach an LLM through a provider adapter (which is what the gate guards).
//
// None of these can pass by doing nothing: each asserts a coverage floor, and
// the registry test also fails on STALE entries, so the census cannot rot.

// gateFunc is the dispatch-time gate every egress-capable provider call must
// pass. TestNoUngatedProviderDispatch asserts it still exists, so renaming it
// fails loudly instead of silently disarming the guard.
const gateFunc = "enforceSovereignty"

// outboundDialSites is the COMPLETE census of non-test source files that can
// open an outbound connection, each with the reason it is allowed to and where
// its egress decision is made. Adding a dialer anywhere in the repo fails
// TestOutboundDialSitesAreRegistered until it is listed here — that is the
// point: the reason has to be written down before the code ships.
//
// GATED means the call is reachable only through Server.enforceSovereignty.
// UNGATED means it is not, and the entry says what makes it acceptable. Keep
// this in sync with docs/architecture.md#what-the-gate-does-not-cover.
var outboundDialSites = map[string]string{
	// GATED — the provider adapters. These are the only paths a prompt can take
	// off the box, and every caller of them runs the gate first (asserted by
	// TestNoUngatedProviderDispatch).
	"core/provider/provider.go":                "GATED: the shared upstream http.Clients used by every adapter; reachable only via gated dispatch",
	"core/provider/passthrough/passthrough.go": "GATED: OpenAI-shaped upstream adapter, dialed only from gated dispatch",
	"core/provider/anthropic/anthropic.go":     "GATED: Anthropic adapter, dialed only from gated dispatch",
	"core/provider/azure/azure.go":             "GATED: Azure adapter, dialed only from gated dispatch",
	"core/provider/bedrock/bedrock.go":         "GATED: Bedrock adapter, dialed only from gated dispatch",
	"core/provider/cohere/cohere.go":           "GATED: Cohere adapter, dialed only from gated dispatch",
	"core/provider/gemini/gemini.go":           "GATED: Gemini adapter, dialed only from gated dispatch",

	// UNGATED — non-inference egress. None of these carries prompt or completion
	// text; each is off unless the operator configures it, EXCEPT the price
	// catalog sync, which is ON by default (see the entry below).
	"core/pricing/source.go": "UNGATED, DEFAULT ON: price-catalog feeds (openrouter.ai, raw.githubusercontent.com). " +
		"config.Default() ships these URLs, so a stock gateway makes an outbound GET at startup and every " +
		"sync_interval_minutes. It sends no prompt, no key and no usage — it is a plain GET of a public price " +
		"list — but it IS off-box traffic the sovereignty gate does not classify. Disable with \"pricing\": " +
		"{\"sources\": []} (see TestPricingSourcesCanBeDisabled in core/config).",
	"integration/cp/cp.go": "UNGATED, OPT-IN: the optional control-plane seam (identity/budget/usage). Dials only " +
		"when LLMUX_CP_URL is set, and sends billing counts only — never prompts or completions. " +
		"core never imports this package.",
	"core/keys/pgstore.go": "UNGATED, OPT-IN: Postgres pool for virtual-key spend. Operator-configured DSN, " +
		"no prompt content.",
	"core/gateway/gateway.go": "UNGATED, OPT-IN: Redis client for shared rate limits + cache. Operator-configured " +
		"address. Cache VALUES can contain completions, so treat Redis as inside your sovereignty boundary. " +
		"This is the only outbound construct allowed in the gateway core or its HTTP shell (see " +
		"TestServerPackageNeverDialsDirectly). It moved here from core/server when dispatch moved into " +
		"core/gateway; the client is built lazily-connected and only Gateway.Start pings it.",

	// UNGATED — clients OF a llmux gateway, not the gateway itself.
	"cmd/llmux/cli.go":       "UNGATED, CLIENT: the CLI subcommands call a llmux gateway (default http://localhost:4000), not a provider.",
	"sdks/go/llmux/llmux.go": "UNGATED, CLIENT: the embedded-gateway SDK polls its own loopback /health to wait for readiness.",
	"sdks/go/examples/sidecar/main.go": "UNGATED, CLIENT + EXAMPLE: the Go SDK's sidecar example. It spawns " +
		"`llmux serve` itself on 127.0.0.1 on a port it just bound, then talks HTTP to THAT — it is a client " +
		"of a llmux gateway, never of a provider, so the sovereignty gate is enforced on the far side of the " +
		"hop by the server it started. Every address it dials is its own child. It is an example under " +
		"sdks/, is not imported by any library or shipped binary, and its sibling sdks/go/examples/direct " +
		"deliberately dials nothing at all (it embeds the gateway instead), which is the contrast the pair " +
		"exists to draw.",

	// UNGATED — test harness.
	"core/conformance/transport.go": "UNGATED, HARNESS: record/replay RoundTripper for provider conformance fixtures. " +
		"Replay (the CI mode) makes no network call; Record/Live require real keys and are opt-in.",
	"ffi/bench/main.go": "UNGATED, HARNESS: the C-ABI latency benchmark (in-process vs loopback HTTP). Every " +
		"address it dials is 127.0.0.1 on a port the same process just bound — its own fake upstream and its " +
		"own llmux HTTP server. It lives in the separate github.com/vul-os/llmux-ffi module, is not part of " +
		"`go build ./...`, and is never linked into libllmux or any shipped binary.",
}

// minDialSites is a coverage floor: if the AST scan silently stopped matching
// (a Go syntax change, a bad walk, a wrong root) it would find zero dialers and
// the registry test would "pass" by checking nothing. Assert it found roughly
// what we know is there.
const minDialSites = 12

// minDispatchSites is the same floor for the dispatch scan: six known call
// sites (unary chat, streaming chat, embeddings, semantic-cache embedder,
// modality forward, transcription forward) as of this writing.
const minDispatchSites = 6

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("egress guard: getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("egress guard: no go.mod above %s — the guard scanned NOTHING and verified NOTHING", start)
		}
		dir = parent
	}
}

// skipDir reports whether a directory holds no first-party Go source we want to
// scan (vendored JS, build output, fixtures).
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "testdata", "assets":
		return true
	}
	return false
}

// goFiles returns every non-test .go file in the repo, as repo-relative paths.
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("egress guard: walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("egress guard: found no Go source under %s — the guard verified NOTHING", root)
	}
	sort.Strings(out)
	return out
}

// parseFile parses one repo-relative Go file.
func parseFile(t *testing.T, root, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil, 0)
	if err != nil {
		t.Fatalf("egress guard: parse %s: %v", rel, err)
	}
	return f
}

// qualified reports whether e is a package-qualified selector pkg.Name.
func qualified(e ast.Expr, pkg string, names ...string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}

// dialConstructs finds the outbound-connection constructs in a parsed file:
// building an outbound HTTP request, constructing (or reaching for) an HTTP
// client/transport, opening a Redis client or a Postgres pool, or dialing a
// socket directly. Inbound constructs (http.Server, net.Listen) are ignored —
// serving is not egress.
func dialConstructs(f *ast.File) []string {
	var found []string
	add := func(s string) {
		for _, existing := range found {
			if existing == s {
				return
			}
		}
		found = append(found, s)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			switch {
			case qualified(v.Fun, "http", "NewRequest", "NewRequestWithContext", "Get", "Post", "Head", "PostForm"):
				add("http request")
			case qualified(v.Fun, "net", "Dial", "DialTimeout"):
				add("net.Dial")
			case qualified(v.Fun, "redis", "NewClient", "NewFailoverClient", "NewClusterClient"):
				add("redis client")
			case qualified(v.Fun, "pgxpool", "New", "NewWithConfig"):
				add("postgres pool")
			}
		case *ast.CompositeLit:
			if qualified(v.Type, "http", "Client", "Transport") {
				add("http client")
			}
		case *ast.SelectorExpr:
			if qualified(v, "http", "DefaultClient", "DefaultTransport") {
				add("http default client/transport")
			}
		}
		return true
	})
	sort.Strings(found)
	return found
}

// TestOutboundDialSitesAreRegistered is the guard that makes a NEW egress path
// fail CI. Every non-test file that can open an outbound connection must appear
// in outboundDialSites with a reason recording whether the sovereignty gate
// covers it. The test also fails on stale entries, so the census cannot drift
// from the code in either direction.
func TestOutboundDialSitesAreRegistered(t *testing.T) {
	root := repoRoot(t)

	found := map[string][]string{}
	for _, rel := range goFiles(t, root) {
		if c := dialConstructs(parseFile(t, root, rel)); len(c) > 0 {
			found[rel] = c
		}
	}

	if len(found) < minDialSites {
		t.Fatalf("egress guard found only %d dialing files (floor %d) — the scan is broken and this test is "+
			"verifying nothing; found=%v", len(found), minDialSites, found)
	}

	for rel, constructs := range found {
		if _, ok := outboundDialSites[rel]; !ok {
			t.Errorf("UNREGISTERED EGRESS PATH: %s opens an outbound connection (%s).\n"+
				"  Every dialer must be declared in outboundDialSites (core/sovereign/egress_guard_test.go) with a\n"+
				"  reason saying whether Server.enforceSovereignty covers it. If this is a new inference path it must\n"+
				"  call the gate (see TestNoUngatedProviderDispatch); if it is not, say so and why it is safe.",
				rel, strings.Join(constructs, ", "))
		}
	}
	for rel := range outboundDialSites {
		if _, ok := found[rel]; !ok {
			t.Errorf("STALE REGISTRY ENTRY: %s is listed in outboundDialSites but no longer dials anything — "+
				"delete the entry so the census keeps meaning something", rel)
		}
	}
}

// egressMethods returns the provider-surface methods that reach the network,
// read from the interfaces themselves (provider.Provider and provider.Forwarder)
// rather than hardcoded. Adding a method to either interface therefore extends
// this guard automatically — a new streaming or batch verb cannot be introduced
// ungated without the test noticing.
func egressMethods(t *testing.T, root string) map[string]bool {
	t.Helper()
	// Name() returns configuration, not a connection.
	nonEgress := map[string]bool{"Name": true}
	out := map[string]bool{}
	for _, spec := range []struct{ file, iface string }{
		{"core/provider/provider.go", "Provider"},
		{"core/provider/forward.go", "Forwarder"},
	} {
		f := parseFile(t, root, spec.file)
		var iface *ast.InterfaceType
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != spec.iface {
				return true
			}
			iface, _ = ts.Type.(*ast.InterfaceType)
			return false
		})
		if iface == nil {
			t.Fatalf("egress guard: interface %s not found in %s — the guard cannot derive the egress surface",
				spec.iface, spec.file)
		}
		for _, m := range iface.Methods.List {
			for _, name := range m.Names {
				if !nonEgress[name.Name] {
					out[name.Name] = true
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("egress guard: derived an EMPTY egress-method set — the guard would match nothing")
	}
	return out
}

// callsGate reports whether a function body calls the sovereignty gate.
func callsGate(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == gateFunc {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestNoUngatedProviderDispatch is the core structural guarantee: every call to
// an egress-capable provider method, anywhere outside the adapters themselves,
// must live in a function that also calls enforceSovereignty. Add a seventh
// dispatch path and forget the gate, and this fails with the file and line.
//
// It is deliberately conservative — the gate must be in the SAME function as
// the dispatch, not somewhere up the call chain — because "a caller two frames
// up checks it" is exactly the reasoning that produces a bypass.
func TestNoUngatedProviderDispatch(t *testing.T) {
	root := repoRoot(t)
	methods := egressMethods(t, root)

	// The gate itself must still exist; otherwise every check below is vacuous.
	gateDecl := false
	ast.Inspect(parseFile(t, root, "core/server/sovereignty.go"), func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok && fn.Name.Name == gateFunc {
			gateDecl = true
		}
		return true
	})
	if !gateDecl {
		t.Fatalf("egress guard: %s is not declared in core/server/sovereignty.go — if it was renamed, update "+
			"gateFunc; until then this guard enforces nothing", gateFunc)
	}

	fset := token.NewFileSet()
	sites, ungated := 0, 0
	for _, rel := range goFiles(t, root) {
		// The adapters under core/provider are where these methods are
		// IMPLEMENTED (and where the dial legitimately happens); the rule is
		// about their CALLERS. The Go SDK is a client of a gateway, not a
		// dispatcher inside one.
		if strings.HasPrefix(rel, "core/provider/") || strings.HasPrefix(rel, "sdks/") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("egress guard: parse %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			gated := callsGate(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !methods[sel.Sel.Name] {
					return true
				}
				sites++
				if !gated {
					ungated++
					t.Errorf("UNGATED DISPATCH: %s:%d calls %s.%s but %s() does not call %s.\n"+
						"  Every provider call that can leave the box must pass the sovereignty gate in the SAME\n"+
						"  function, before the call (see core/server/dispatch.go for the pattern).",
						rel, fset.Position(call.Pos()).Line, exprString(sel.X), sel.Sel.Name, fn.Name.Name, gateFunc)
				}
				return true
			})
		}
	}

	if sites < minDispatchSites {
		t.Fatalf("egress guard found only %d provider-dispatch call sites (floor %d) — the scan is broken and "+
			"this test is verifying nothing", sites, minDispatchSites)
	}
	t.Logf("egress guard: %d provider-dispatch call sites checked, %d ungated", sites, ungated)
}

// exprString renders a receiver expression for error messages (best effort).
func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprString(v.Fun) + "()"
	case *ast.IndexExpr:
		return exprString(v.X) + "[…]"
	}
	return "?"
}

// dispatchPackages are the packages that decide where a request goes: the
// gateway core (core/gateway, which owns dispatch, retries and failover) and
// the HTTP shell over it (core/server, which still owns the modality and
// transcription forwards). Both are covered by the no-direct-dialing rule.
var dispatchPackages = []string{"core/gateway/", "core/server/"}

// TestServerPackageNeverDialsDirectly keeps the gateway's own dispatch on the
// gated path. Neither core/gateway nor core/server may reach an LLM except
// through a provider adapter; if either ever builds its own http.Request it
// would bypass the gate by construction and no behavioral test would notice.
// The one allowed exception (the Redis client in core/gateway/gateway.go) is
// named explicitly.
func TestServerPackageNeverDialsDirectly(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{"core/gateway/gateway.go": true} // redis.NewClient only

	checked := 0
	for _, rel := range goFiles(t, root) {
		inScope := false
		for _, pkg := range dispatchPackages {
			if strings.HasPrefix(rel, pkg) {
				inScope = true
				break
			}
		}
		if !inScope {
			continue
		}
		checked++
		constructs := dialConstructs(parseFile(t, root, rel))
		if len(constructs) == 0 {
			continue
		}
		if allowed[rel] {
			// The exception is Redis, not HTTP: an http.Client here would still
			// be a bypass.
			for _, c := range constructs {
				if strings.HasPrefix(c, "http") {
					t.Errorf("BYPASS: %s builds an outbound HTTP construct (%s). The gateway must reach upstreams "+
						"only through a provider adapter, which is what the sovereignty gate guards.", rel, c)
				}
			}
			continue
		}
		t.Errorf("BYPASS: %s opens an outbound connection (%s). Dispatch must go through a provider "+
			"adapter so %s applies.", rel, strings.Join(constructs, ", "), gateFunc)
	}
	if checked == 0 {
		t.Fatal("egress guard: scanned no core/gateway or core/server files — this test verified nothing")
	}
	t.Logf("egress guard: %d dispatch-package files checked for direct dialing", checked)
}

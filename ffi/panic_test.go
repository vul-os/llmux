package main

// The panic backstop.
//
// In c-shared mode a panic that escapes an //export function is not an error a
// host can handle — it is a runtime fatal error that ends the host's process.
// The HTTP shell has always had a recover (core/server.recoverMW), so the same
// bug that is a logged 500 in the sidecar was a dead worker in the library, in
// two deployments the README calls interchangeable.
//
// These tests drive a real panic through each ABI entry point and assert the
// call RETURNS an error and the test binary is still alive afterwards. A test
// process that survives to the end of the run is itself part of the assertion:
// without the recover, `go test` reports the whole package as a crash.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
)

// corruptHandle registers an instance whose gateway is nil. Every dispatch path
// dereferences it, so this is a real panic raised by real production code
// rather than a panic the test threw at a seam.
func corruptHandle(t *testing.T) uint64 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{gw: nil, ctx: ctx, cancel: cancel}
	h := nextID.Add(1)
	regMu.Lock()
	reg[h] = inst
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		delete(reg, h)
		regMu.Unlock()
		cancel()
	})
	return h
}

func assertPanicError(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s returned no error after a panic — the panic either did not happen "+
			"(this test now verifies nothing) or was swallowed silently", what)
	}
	if !strings.Contains(err.Error(), "recovered from a panic") {
		t.Errorf("%s error = %q, want the recovered-panic diagnosis so the host can tell a "+
			"library bug from a bad request", what, err)
	}
	if !strings.Contains(err.Error(), "goroutine ") {
		t.Errorf("%s error carries no stack; a shared library has no log of its own, so the "+
			"stack has nowhere to go except the error string:\n%v", what, err)
	}
}

func TestNewRecoversAPanicInsteadOfKillingTheHost(t *testing.T) {
	orig := newGateway
	newGateway = func(*config.Config, ...gateway.Option) (*gateway.Gateway, error) {
		panic("boom during construction")
	}
	t.Cleanup(func() { newGateway = orig })

	before := liveHandles()
	h, err := openGateway("")
	assertPanicError(t, err, "openGateway")
	if h != 0 {
		t.Errorf("openGateway returned handle %d after a panic; the ABI's failure value is 0", h)
	}
	if liveHandles() != before {
		t.Errorf("a panicking openGateway left %d handles registered (was %d)", liveHandles(), before)
	}
}

func TestCallRecoversAPanicInsteadOfKillingTheHost(t *testing.T) {
	h := corruptHandle(t)
	for _, method := range []string{"models", "chat", "embed"} {
		body := `{"model":"demo","messages":[{"role":"user","content":"x"}],"input":"x"}`
		out, err := callMethod(h, method, body)
		assertPanicError(t, err, "callMethod "+method)
		if out != "" {
			t.Errorf("callMethod %s returned %q alongside the panic error; the ABI's failure "+
				"value is a NULL result", method, out)
		}
	}
}

func TestStreamRecoversAPanicInsteadOfKillingTheHost(t *testing.T) {
	h := corruptHandle(t)
	err := streamMethod(h, "chat", `{"model":"demo","messages":[]}`,
		func(string) error { return nil })
	assertPanicError(t, err, "streamMethod")
}

// The host's chunk callback runs inside llmux_stream's own call frame. A
// binding whose trampoline panics — a Python binding that failed to reacquire
// the GIL, a JVM binding on a thread that was never attached — must come back
// as an error, not as a fatal runtime error in the host.
func TestStreamRecoversAPanicFromTheHostCallback(t *testing.T) {
	h, _ := newTestHandle(t, "one two three four")
	err := streamMethod(h, "chat", `{"model":"demo","messages":[{"role":"user","content":"go"}]}`,
		func(string) error { panic("the host's callback blew up") })
	assertPanicError(t, err, "streamMethod with a panicking callback")
	if abortedError(err) {
		t.Error("a panicking callback was reported as a clean host-requested abort; the host " +
			"did not ask to stop, its code broke")
	}
}

func TestCloseRecoversAPanicInsteadOfKillingTheHost(t *testing.T) {
	h := corruptHandle(t)
	before := liveHandles()
	closeGateway(h) // must not take the process down
	if liveHandles() != before-1 {
		t.Errorf("liveHandles = %d after closing a handle whose teardown panicked, want %d — "+
			"the entry leaked and the host can never clean it up", liveHandles(), before-1)
	}
	if _, err := callMethod(h, "models", ""); err == nil || !strings.Contains(err.Error(), "unknown handle") {
		t.Errorf("after a panicking close the handle answered %v, want an unknown-handle error", err)
	}
}

// recovered's own contract, asserted directly: an error, never nil, whatever
// the panic value was.
func TestRecoveredNeverReturnsNil(t *testing.T) {
	for _, v := range []any{nil, "s", 42, errors.New("e")} {
		if err := recovered(v); err == nil {
			t.Errorf("recovered(%v) = nil; a nil error at an entry point reads as success", v)
		}
	}
}

// The Go tests above cover abi.go's entry points, which is where every panic
// this library can actually raise is caught. The //export functions in
// cshared.go carry a second defer for panics in the cgo marshalling itself, and
// those cannot be called from a Go test (cgo is not supported in _test.go
// files) nor provoked from C. So this reads the source: an //export added
// without a backstop fails here rather than in someone's production process.
func TestEveryExportHasAPanicBackstop(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cshared.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse cshared.go: %v", err)
	}

	found := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Doc == nil || fn.Body == nil {
			continue
		}
		exported := false
		for _, c := range fn.Doc.List {
			if strings.HasPrefix(c.Text, "//export ") {
				exported = true
			}
		}
		if !exported {
			continue
		}
		found++
		if !recoversSomewhere(fn.Body) {
			t.Errorf("//export %s has no deferred recover(). A panic escaping it is a runtime "+
				"fatal error that kills the HOST process, not an error the host can handle.", fn.Name.Name)
		}
	}
	// Coverage floor: without it, a rename of the //export convention would make
	// the loop above examine nothing and still pass.
	if found != 6 {
		t.Fatalf("found %d //export functions in cshared.go, expected 6 — the scan is wrong, or "+
			"the ABI grew a symbol and this floor was not updated", found)
	}
}

// recoversSomewhere reports whether body contains a defer whose function calls
// recover().
func recoversSomewhere(body *ast.BlockStmt) bool {
	got := false
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "recover" {
				got = true
			}
			return true
		})
		return true
	})
	return got
}

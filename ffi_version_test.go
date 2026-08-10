package llmux_test

// llmux_abi_version reports the release the shared library was built from, so a
// host can spot a stale .so on its load path. That string used to be a
// hand-typed `const Version` in ffi/abi.go, and in v0.1.3 it came apart from
// /VERSION: the release was tagged and pushed with the constant a patch behind,
// so the library told every host it was old when it was current. openrate
// shipped the identical defect in the same release, from the same cause. Every
// check run before tagging was green, because ffi/ is a SEPARATE Go module and
// `go test ./...` here cannot descend into it — the pin that would have caught
// the drift lived inside the module that was wrong.
//
// The fix is structural: ffi/abi.go no longer declares a version, it derives one
// from llmux.Version, which is `go:embed`ed from VERSION at compile time. There
// is no second copy left to forget.
//
// These tests are the belt to that braces. They live in the ROOT module on
// purpose and read ffi/'s source as TEXT rather than importing it, because
// importing across the module boundary is exactly what is not possible — and so
// that the day someone types a literal back into ffi/abi.go, the root module's
// own `go test ./...` says so, without anyone having to remember that
// `make test-ffi` is a separate command.
//
// The scan parses the file rather than grepping it, so that a version-shaped
// string in a COMMENT — including the ones above, which have to be able to
// quote the defect they describe — is not mistaken for a declaration.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vul-os/llmux"
)

// TestVersionIsTheVERSIONFile checks the embed points at what we think it does.
// Everything else derives from llmux.Version; if the embed were aimed at the
// wrong file, every other assertion here would agree with each other and be
// wrong together.
func TestVersionIsTheVERSIONFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	want := strings.TrimSpace(string(raw))
	if want == "" {
		t.Fatal("VERSION is empty, so this test would compare nothing against nothing")
	}
	if llmux.Version != want {
		t.Errorf("llmux.Version is %q but the VERSION file on disk says %q", llmux.Version, want)
	}
}

// semverLiteral is a string that looks like a release number.
var semverLiteral = regexp.MustCompile(`^v?\d+\.\d+\.\d+`)

// TestFFIDerivesItsABIVersionRatherThanDeclaringOne is the guard that replaced
// the old string comparison. Comparing two copies of a version can only detect
// drift after someone has written it; this asserts there is no second copy that
// COULD drift, which is the property that actually holds now.
func TestFFIDerivesItsABIVersionRatherThanDeclaringOne(t *testing.T) {
	const path = "ffi/abi.go"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// 1. The declaration is a derivation from the library's own version.
	var found ast.Expr
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.VAR && gd.Tok != token.CONST) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name == "Version" && i < len(vs.Values) {
					found = vs.Values[i]
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("%s declares no Version. llmux_abi_version has nothing to report, or this scan "+
			"is looking in the wrong place — either way, fix it rather than deleting the test.", path)
	}
	sel, ok := found.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Version" {
		t.Errorf("%s initialises Version from %T, not from llmux.Version.\n"+
			"llmux_abi_version must report the VERSION file of the tree the library was built "+
			"from, and the only way to guarantee that is to derive it. If this was refactored "+
			"rather than reverted, teach this test the new shape — do not replace the derivation "+
			"with a literal.", path, found)
	} else if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "llmux" {
		t.Errorf("%s initialises Version from %s.Version, not llmux.Version", path, sel.X)
	}

	// 2. No version-shaped literal survives anywhere in the file, whether or not
	//    the derivation above is still standing next to it.
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil || !semverLiteral.MatchString(s) {
			return true
		}
		t.Errorf("%s:%d contains the version-shaped literal %q.\n"+
			"That is the exact shape of the v0.1.3 defect: a release bumps VERSION, this does "+
			"not, and the shared library misreports itself to every host that asks. Derive it "+
			"from llmux.Version.", path, fset.Position(lit.Pos()).Line, s)
		return true
	})
}

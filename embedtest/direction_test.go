package embedtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// G3 — import direction. The cheapest guard on the list and the one that stops
// the shape rotting back: core/gateway is the library, core/server is one shell
// over it, and web/ is the admin console. Dependencies point shell -> library,
// never the other way. A single `import "github.com/vul-os/llmux/core/server"`
// added to the library for convenience would undo the whole refactor, compile
// fine, and pass every behavioural test in the repo.

const (
	libPkg    = "github.com/vul-os/llmux/core/gateway"
	serverPkg = "github.com/vul-os/llmux/core/server"
	uiPkg     = "github.com/vul-os/llmux/web"
)

// repoRoot is the main module's directory (embedtest is a nested module).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s — this guard would have scanned nothing: %v", root, err)
	}
	return root
}

// deps returns `go list -deps` for pkg in the main module, under the given
// build tags.
func deps(t *testing.T, tags string, pkg string) []string {
	t.Helper()
	args := []string{"list", "-deps"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	var list []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			list = append(list, line)
		}
	}
	if len(list) == 0 {
		t.Fatalf("go %s produced no dependency list — this guard verified NOTHING", strings.Join(args, " "))
	}
	return list
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestGatewayDependsOnNeitherServerNorUI is the inversion check.
func TestGatewayDependsOnNeitherServerNorUI(t *testing.T) {
	for _, tags := range []string{"", "noui"} {
		name := "default build"
		if tags != "" {
			name = "-tags " + tags
		}
		t.Run(name, func(t *testing.T) {
			list := deps(t, tags, libPkg)

			// Coverage floor: the scan must have found the library's real
			// dependency graph, or "the forbidden package is absent" is vacuous.
			for _, must := range []string{
				"github.com/vul-os/llmux/core/provider",
				"github.com/vul-os/llmux/core/router",
				"github.com/vul-os/llmux/core/config",
				"github.com/vul-os/llmux/core/sovereign",
			} {
				if !contains(list, must) {
					t.Fatalf("go list -deps %s (%d packages) does not include %s — the list is wrong and "+
						"the assertions below mean nothing", libPkg, len(list), must)
				}
			}

			failed := false
			if contains(list, serverPkg) {
				failed = true
				t.Errorf("INVERTED IMPORT: %s depends on %s. The library must not depend on its HTTP shell — "+
					"an embedder would then link the whole server, its routes and its middlewares.", libPkg, serverPkg)
			}
			if contains(list, uiPkg) {
				failed = true
				t.Errorf("INVERTED IMPORT: %s depends on %s. The library must not depend on the admin console "+
					"embed — see G7, which asserts the resulting binary carries no UI bytes.", libPkg, uiPkg)
			}
			if !failed {
				t.Logf("%s: %d transitive packages, neither %s nor %s among them", libPkg, len(list), serverPkg, uiPkg)
			}
		})
	}
}

// Positive control for the check above: the shell really does depend on both,
// so the matcher can see these package paths when they ARE present. Without
// this, a typo in serverPkg/uiPkg would make the inversion check permanently,
// silently green.
func TestServerDependsOnGatewayAndUI(t *testing.T) {
	list := deps(t, "", serverPkg)
	if !contains(list, libPkg) {
		t.Errorf("CONTROL FAILED: %s does not depend on %s — the shell is supposed to be built ON the library",
			serverPkg, libPkg)
	}
	if !contains(list, uiPkg) {
		t.Errorf("CONTROL FAILED: %s does not depend on %s, so the absence of %s in the library's deps is not "+
			"evidence of anything — the matcher may simply never match", serverPkg, uiPkg, uiPkg)
	}
}

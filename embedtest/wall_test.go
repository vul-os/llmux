package embedtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// G1's mechanism, executed rather than asserted in a comment.
//
// Every other test in this module rests on one claim: "if it compiles, it used
// only llmux's exported API". That claim is NOT a property of being a separate
// module. Go's internal rule is about IMPORT PATH PREFIXES:
//
//	an import of a path containing "internal" is allowed only from code whose
//	import path begins with the prefix up to that "internal" element.
//
// So a test module named github.com/vul-os/llmux/embedtest — the obvious name,
// and the one this suite originally used — sits UNDER the github.com/vul-os/llmux
// prefix and may import github.com/vul-os/llmux/internal/... freely. The whole
// guard suite would then be a false green. Measured here before the rename: with
// the module named .../llmux/embedtest, `go build -tags internalwall
// ./internalprobe` exited 0. After renaming to github.com/vul-os/llmux-embedtest
// it exits 1 with "use of internal package ... not allowed".
//
// This test keeps that difference under CI instead of in a commit message.

const wallProbeTarget = "github.com/vul-os/llmux/internal/embedwall"

// TestInternalWallRefusesAnInternalImport builds internalprobe/ — whose only
// content is a blank import of a main-module internal package — and REQUIRES
// the compiler to refuse it.
func TestInternalWallRefusesAnInternalImport(t *testing.T) {
	// Premise: the target package must actually exist and be a valid import
	// path inside the main module. Otherwise the build below would fail for a
	// boring reason (typo, deleted package) and this test would report a wall
	// that is not there.
	list := exec.Command("go", "list", wallProbeTarget)
	list.Dir = repoRoot(t)
	if out, err := list.CombinedOutput(); err != nil {
		t.Fatalf("the probe target %s does not resolve inside the main module: %v\n%s\n"+
			"Restore internal/embedwall, or point wallProbeTarget at another main-module internal package. "+
			"Until then this guard cannot tell a wall from a typo.", wallProbeTarget, err, out)
	}

	build := exec.Command("go", "build", "-tags", "internalwall", "./internalprobe")
	build.Dir = mustAbs(t, ".")
	out, err := build.CombinedOutput()
	if err == nil {
		t.Fatalf("THE WALL IS GONE: this module compiled a blank import of %s.\n"+
			"  Every other test here claims to prove llmux's PUBLIC API is sufficient; that claim is now "+
			"unfounded, because these tests can reach main-module internals.\n"+
			"  The usual cause is the module path: it must NOT begin with github.com/vul-os/llmux/ (see "+
			"embedtest/go.mod). Go's internal rule compares import-path prefixes, not modules.", wallProbeTarget)
	}
	if !strings.Contains(string(out), "use of internal package") {
		t.Fatalf("the probe failed to build, but not because of the internal rule — so this test proves "+
			"nothing about the wall:\n%s", out)
	}
	t.Logf("wall holds: %s", strings.TrimSpace(string(out)))
}

// The module path is the thing that actually decides the above. Assert it
// directly too, so a rename gets a one-line diagnosis instead of a build error
// to interpret.
func TestModulePathSitsOutsideTheLibraryPrefix(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(mustAbs(t, "."), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var path string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "module ") {
			path = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			break
		}
	}
	if path == "" {
		t.Fatal("no module directive in embedtest/go.mod")
	}
	if strings.HasPrefix(path, "github.com/vul-os/llmux/") {
		t.Fatalf("module path %q sits UNDER the library's import-path prefix, so this module may import "+
			"github.com/vul-os/llmux/internal/... and the embeddability claim collapses. Use a path outside "+
			"the prefix (github.com/vul-os/llmux-embedtest).", path)
	}
	t.Logf("module path %q is outside github.com/vul-os/llmux/", path)
}

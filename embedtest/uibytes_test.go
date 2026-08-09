package embedtest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// G7 — importing the library links NO UI bytes.
//
// G3 proves the library does not IMPORT the console package. This proves the
// consequence an embedder actually cares about: the compiled binary of a host
// that uses llmux as a library does not contain the console's HTML at all.
// The two are not the same claim — a stray `var _ = webui.HTML` in any package
// the library reaches would satisfy neither, but only this one measures the
// artefact that ships.

// uiSentinels are byte strings that exist ONLY inside web/ui.html. They are
// verified to be present in the source file before being searched for in a
// binary, so an edit that renames them fails loudly instead of quietly turning
// the search into "look for something that no longer exists".
var uiSentinels = []string{
	"<title>llmux — admin</title>",
	"--accent-hover:#5EEAD4",
}

func assertSentinelsExistInSource(t *testing.T) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "web", "ui.html"))
	if err != nil {
		t.Fatalf("read web/ui.html: %v", err)
	}
	for _, s := range uiSentinels {
		if !bytes.Contains(src, []byte(s)) {
			t.Fatalf("sentinel %q is no longer in web/ui.html — this guard would be searching binaries for "+
				"a string that cannot be there, and would pass no matter what got linked. Pick a new sentinel.", s)
		}
	}
}

// buildHost compiles one of the embedtest host programs and returns its path.
func buildHost(t *testing.T, pkg string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "host")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Dir = mustAbs(t, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %s: %v", p, err)
	}
	return abs
}

func binaryHasSentinels(t *testing.T, bin string) (found []string, size int64) {
	t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read %s: %v", bin, err)
	}
	for _, s := range uiSentinels {
		if bytes.Contains(data, []byte(s)) {
			found = append(found, s)
		}
	}
	return found, int64(len(data))
}

// TestLibraryOnlyHostLinksNoUIBytes builds a host that imports only the library
// and asserts the console is nowhere in it — with a positive control that the
// same search DOES find the console in a host that mounts it.
func TestLibraryOnlyHostLinksNoUIBytes(t *testing.T) {
	assertSentinelsExistInSource(t)

	libBin := buildHost(t, "./hosts/libonly")
	libFound, libSize := binaryHasSentinels(t, libBin)
	if len(libFound) > 0 {
		t.Errorf("UI BYTES IN A LIBRARY-ONLY HOST: ./hosts/libonly links %v. Importing core/gateway must not "+
			"drag in web/ — check for a new import of the console package anywhere in the library's graph "+
			"(G3 catches the import; this catches the bytes).", libFound)
	}

	uiBin := buildHost(t, "./hosts/withui")
	uiFound, uiSize := binaryHasSentinels(t, uiBin)
	if len(uiFound) != len(uiSentinels) {
		t.Fatalf("CONTROL FAILED: a host that mounts the console contains only %d of %d sentinels (%v). "+
			"The search cannot see embedded bytes, so the assertion above proved nothing.",
			len(uiFound), len(uiSentinels), uiFound)
	}

	if uiSize <= libSize {
		t.Errorf("the console-mounting host (%d bytes) is not larger than the library-only host (%d bytes) — "+
			"the console is apparently free, which it is not", uiSize, libSize)
	}
	t.Logf("library-only host %d bytes, no UI bytes; library+server(UI) host %d bytes, all %d sentinels present "+
		"(delta %d bytes)", libSize, uiSize, len(uiSentinels), uiSize-libSize)
}

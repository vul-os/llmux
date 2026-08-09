package embedtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// G5 — both UI build states compile, and `-tags noui` is not a no-op.
//
// The failure this is written against: a `noui` build that still links the
// embed. It compiles, it passes every test, and the only symptom is a binary
// that is exactly as big as before — which nothing was measuring. So this
// builds the real binary both ways and asserts a number, then runs both and
// asserts what /ui/ actually answers.

// noUISizeFloor is the minimum number of bytes `-tags noui` must remove.
//
// Measured on this tree (darwin/arm64, go1.25.12):
//
//	default 17,949,410  ->  noui 17,916,098  =  33,312 bytes saved
//
// The two embedded files are 21,509 bytes (web/ui.html) and 15,142 bytes
// (web/THIRD-PARTY-NOTICES-GO.txt) = 36,651 bytes, so the saving is the embed
// and little else. The floor is set below the measurement (not at it) because
// linker output moves with the toolchain and the platform; it is set high
// enough that dropping the embed is the only way to clear it.
const noUISizeFloor = 20000

// buildBinary builds a package from the MAIN module (not embedtest) with the
// given tags, and returns the path and size of the artefact.
func buildBinary(t *testing.T, tags, out string) (string, int64) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), out)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := []string{"build", "-o", bin}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./cmd/llmux")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot(t)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build (tags=%q): %v\n%s", tags, err, o)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat %s: %v", bin, err)
	}
	return bin, fi.Size()
}

func TestNoUIBuildDropsTheEmbedAndIsSmaller(t *testing.T) {
	assertSentinelsExistInSource(t)

	defBin, defSize := buildBinary(t, "", "llmux")
	nouiBin, nouiSize := buildBinary(t, "noui", "llmux-noui")

	defFound, _ := binaryHasSentinels(t, defBin)
	if len(defFound) != len(uiSentinels) {
		t.Fatalf("the DEFAULT build contains only %d of %d console sentinels (%v) — the console is supposed "+
			"to be embedded by default, and without it the noui comparison below is meaningless",
			len(defFound), len(uiSentinels), defFound)
	}

	nouiFound, _ := binaryHasSentinels(t, nouiBin)
	if len(nouiFound) > 0 {
		t.Errorf("-tags noui STILL LINKS THE CONSOLE: found %v in the binary. The tag switched the API "+
			"(webui.Enabled() is false) without dropping the bytes.", nouiFound)
	}

	saved := defSize - nouiSize
	t.Logf("cmd/llmux: default %d bytes, -tags noui %d bytes, saved %d bytes", defSize, nouiSize, saved)
	if saved < noUISizeFloor {
		t.Errorf("-tags noui saved only %d bytes (floor %d). web/ui.html is %d bytes on its own, so a saving "+
			"this small means the embed is still linked in some form.", saved, noUISizeFloor, uiHTMLSize(t))
	}
}

func uiHTMLSize(t *testing.T) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(repoRoot(t), "web", "ui.html"))
	if err != nil {
		t.Fatalf("stat web/ui.html: %v", err)
	}
	return fi.Size()
}

// The size delta says the bytes are gone. This says the running binary behaves
// as documented in both states: the default serves the console at /ui/, and the
// noui build answers the machine-readable stub (501 + code "ui_not_built")
// rather than a 404 or a blank page.
func TestBothBuildStatesServeTheDocumentedUIResponse(t *testing.T) {
	defBin, _ := buildBinary(t, "", "llmux")
	nouiBin, _ := buildBinary(t, "noui", "llmux-noui")

	t.Run("default build serves the console", func(t *testing.T) {
		body, status, ctype := getUI(t, defBin)
		if status != http.StatusOK {
			t.Fatalf("GET /ui/ = %d, want 200\n%s", status, body)
		}
		if ctype != "text/html; charset=utf-8" {
			t.Errorf("GET /ui/ Content-Type = %q, want HTML", ctype)
		}
		for _, s := range uiSentinels {
			if !strings.Contains(body, s) {
				t.Errorf("GET /ui/ did not serve the console: sentinel %q missing", s)
			}
		}
	})

	t.Run("noui build serves the JSON stub", func(t *testing.T) {
		body, status, ctype := getUI(t, nouiBin)
		if status != http.StatusNotImplemented {
			t.Fatalf("GET /ui/ = %d on a noui build, want 501\n%s", status, body)
		}
		if ctype != "application/json" {
			t.Errorf("GET /ui/ Content-Type = %q, want JSON", ctype)
		}
		var parsed struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("stub body is not JSON: %v\n%s", err, body)
		}
		if parsed.Error.Code != "ui_not_built" {
			t.Errorf("stub error.code = %q, want %q (body: %s)", parsed.Error.Code, "ui_not_built", body)
		}
	})
}

// getUI boots the given llmux binary on a free loopback port with a hermetic
// config (no providers, no price feeds — nothing leaves the machine) and
// returns what GET /ui/ answers.
func getUI(t *testing.T, bin string) (body string, status int, contentType string) {
	t.Helper()
	addr := freeAddr(t)
	cfgPath := filepath.Join(t.TempDir(), "llmux.json")
	cfg := fmt.Sprintf(`{"server":{"addr":%q},"providers":[],"routes":[],"pricing":{"sources":[]}}`, addr)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "serve", "-config", cfgPath)
	// A deliberately bare environment: a stray OPENAI_API_KEY in the developer's
	// shell would otherwise be auto-detected into the config and dialled by a
	// test that is only asking what /ui/ returns.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	out, err := os.CreateTemp(t.TempDir(), "llmux-log")
	if err != nil {
		t.Fatalf("temp log: %v", err)
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	url := "http://" + addr + "/ui/"
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return string(b), resp.StatusCode, resp.Header.Get("Content-Type")
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(out.Name())
			t.Fatalf("%s never answered %s within 20s: %v\nprocess output:\n%s", filepath.Base(bin), url, err, logs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

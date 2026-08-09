package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vul-os/llmux-ffi/internal/fake"
)

// These tests exercise the ABI's behaviour in pure Go — no cgo, no built
// library. They are the fast half of the evidence. The other half is
// ctest/smoke.c, which dlopens the real .so/.dylib and proves the C symbols
// exist, are callable, and behave the same; neither test replaces the other.
// A Go test cannot catch a missing //export, and a C test that fails gives a
// much worse diagnosis of a logic bug.

// ---------------------------------------------------------------------------
// Version / provenance
// ---------------------------------------------------------------------------

// The whole point of llmux_abi_version is that a host can compare the loaded
// library against the version it expects. That is worthless if the constant
// drifts from the release it claims. The C smoke test performs the same
// comparison against the BUILT library; this one keeps the source honest.
func TestABIVersionMatchesThePackageVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "VERSION"))
	if err != nil {
		t.Fatalf("read ../VERSION: %v — without it this test verifies nothing", err)
	}
	want := strings.TrimSpace(string(data))
	if want == "" {
		t.Fatal("../VERSION is empty; the comparison below would be vacuous")
	}
	if Version != want {
		t.Errorf("ffi Version = %q but ../VERSION says %q.\n"+
			"  llmux_abi_version is how a host detects a stale shared library on its load path. "+
			"A constant that lags the release makes every such check report the wrong answer.", Version, want)
	}
}

// ---------------------------------------------------------------------------
// The wall: this module is a third-party embedder, not an insider
// ---------------------------------------------------------------------------

// The C ABI must be buildable on exactly the API any other embedder has. Go
// enforces that through import-path prefixes, so the property is really a
// property of the module path — assert it directly, and assert the resulting
// dependency graph, so a rename gets a one-line diagnosis.
func TestFFIUsesOnlyThePublicAPI(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var modPath string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "module ") {
			modPath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			break
		}
	}
	if modPath == "" {
		t.Fatal("no module line in go.mod")
	}
	if strings.HasPrefix(modPath, "github.com/vul-os/llmux/") {
		t.Fatalf("module path %q sits UNDER the library's import-path prefix.\n"+
			"  Go's internal rule compares prefixes, so this module could now import "+
			"github.com/vul-os/llmux/internal/... and the C ABI would no longer be evidence that "+
			"llmux's exported surface is sufficient for an embedder.", modPath)
	}

	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps .: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	// Coverage floor: if the list does not contain the library, "no internal
	// package is present" is a statement about an empty set.
	found := false
	for _, d := range deps {
		if d == "github.com/vul-os/llmux/core/gateway" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("go list -deps . (%d packages) does not include core/gateway — the scan is wrong "+
			"and the assertion below means nothing", len(deps))
	}
	for _, d := range deps {
		if strings.HasPrefix(d, "github.com/vul-os/llmux/internal/") {
			t.Errorf("the C ABI depends on %s, a main-module internal package", d)
		}
	}
}

// ---------------------------------------------------------------------------
// Handle registry
// ---------------------------------------------------------------------------

func TestUnknownHandleIsACleanError(t *testing.T) {
	if _, err := callMethod(4242, "models", ""); err == nil {
		t.Fatal("callMethod accepted a handle that was never created")
	} else if !strings.Contains(err.Error(), "unknown handle") {
		t.Errorf("error = %v, want an unknown-handle diagnosis", err)
	}
	err := streamMethod(4242, "chat", `{"model":"demo"}`, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "unknown handle") {
		t.Errorf("streamMethod on an unknown handle: %v, want an unknown-handle diagnosis", err)
	}
}

func TestCloseIsIdempotentAndHandlesAreNotReused(t *testing.T) {
	before := liveHandles()
	h1, err := openGateway("")
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}
	if liveHandles() != before+1 {
		t.Fatalf("liveHandles = %d, want %d — the handle was not registered", liveHandles(), before+1)
	}
	closeGateway(h1)
	closeGateway(h1) // must not panic, must not double-Close the gateway
	if liveHandles() != before {
		t.Errorf("liveHandles = %d after close, want %d — the entry leaked", liveHandles(), before)
	}
	if _, err := callMethod(h1, "models", ""); err == nil {
		t.Error("a closed handle still answered — use-after-close must be an error")
	}
	h2, err := openGateway("")
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}
	defer closeGateway(h2)
	if h2 == h1 {
		t.Errorf("handle %d was reused after close; a stale handle would silently land on a "+
			"different gateway instead of erroring", h2)
	}
}

func TestNewRejectsBadConfigAndReturnsNoHandle(t *testing.T) {
	before := liveHandles()
	if _, err := openGateway(`{"providers": [`); err == nil {
		t.Fatal("openGateway accepted a truncated configuration document")
	}
	if _, err := openGateway(`{"routes":[{"model":"m","provider":"nope"}]}`); err == nil {
		t.Fatal("openGateway accepted a route naming an undefined provider — Validate did not run")
	}
	if liveHandles() != before {
		t.Errorf("a failed openGateway left %d handles registered (was %d)", liveHandles(), before)
	}
}

// ---------------------------------------------------------------------------
// Methods, end to end against the fake upstream
// ---------------------------------------------------------------------------

func newTestHandle(t *testing.T, text string) (uint64, *fakeServer) {
	t.Helper()
	fs := startFake(t, text)
	h, err := openGateway(fs.configJSON)
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}
	t.Cleanup(func() { closeGateway(h) })
	return h, fs
}

func TestChatCallReturnsTheHTTPAPIsJSON(t *testing.T) {
	h, fs := newTestHandle(t, "hello from the fake upstream")

	out, err := callMethod(h, "chat", `{"model":"demo","messages":[{"role":"user","content":"ping"}]}`)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	var resp struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", resp.Object)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from the fake upstream" {
		t.Errorf("content = %+v, want the upstream's answer", resp.Choices)
	}
	if resp.Model != "upstream-model" {
		t.Errorf("model = %q, want the ROUTED target model — routing did not happen", resp.Model)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens == 0 {
		t.Error("usage missing from the result; the metering path did not run")
	}
	if fs.up.ChatHits() != 1 {
		t.Errorf("upstream saw %d chat requests, want 1", fs.up.ChatHits())
	}
}

func TestChatCallRefusesStreamTrue(t *testing.T) {
	h, _ := newTestHandle(t, "x")
	_, err := callMethod(h, "chat", `{"model":"demo","stream":true,"messages":[]}`)
	if err == nil {
		t.Fatal("llmux_call served a stream:true request as a unary blob")
	}
	if !strings.Contains(err.Error(), "llmux_stream") {
		t.Errorf("error = %v, want it to name llmux_stream", err)
	}
}

func TestEmbedCall(t *testing.T) {
	h, fs := newTestHandle(t, "x")
	out, err := callMethod(h, "embed", `{"model":"demo","input":"hello"}`)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	if resp.Object != "list" || len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 3 {
		t.Errorf("unexpected embeddings response: %s", out)
	}
	if fs.up.EmbedHits() != 1 {
		t.Errorf("upstream saw %d embeddings requests, want 1", fs.up.EmbedHits())
	}
}

func TestModelsCall(t *testing.T) {
	h, _ := newTestHandle(t, "x")
	out, err := callMethod(h, "models", "")
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	var resp struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
}

func TestUnknownMethodsAreRejected(t *testing.T) {
	h, _ := newTestHandle(t, "x")
	if _, err := callMethod(h, "transcribe", "{}"); err == nil {
		t.Error("callMethod accepted an unknown method")
	}
	if _, err := callMethod(h, "", "{}"); err == nil {
		t.Error("callMethod accepted an empty method")
	}
	// Streaming accepts a strictly smaller set; "models" is valid for call and
	// must not be valid here.
	err := streamMethod(h, "models", "{}", func(string) error { return nil })
	if err == nil {
		t.Error("streamMethod accepted the models method")
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

func TestStreamDeliversChunksThatReassembleTheAnswer(t *testing.T) {
	h, fs := newTestHandle(t, "one two three four")

	var chunks []string
	var text strings.Builder
	err := streamMethod(h, "chat", `{"model":"demo","messages":[{"role":"user","content":"go"}]}`,
		func(chunk string) error {
			chunks = append(chunks, chunk)
			var c struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(chunk), &c); err != nil {
				return err
			}
			for _, ch := range c.Choices {
				text.WriteString(ch.Delta.Content)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(chunks) < 4 {
		t.Errorf("got %d chunks for a four-word answer — this was not streamed, it was delivered whole", len(chunks))
	}
	if got := text.String(); got != "one two three four" {
		t.Errorf("reassembled text = %q, want %q", got, "one two three four")
	}
	if fs.up.StreamHits() != 1 || fs.up.ChatHits() != 0 {
		t.Errorf("upstream saw %d streaming and %d unary requests, want 1 and 0",
			fs.up.StreamHits(), fs.up.ChatHits())
	}
}

// A callback that says stop must stop the stream, and must NOT be reported to
// the host as a failure — the host asked for it.
func TestStreamAbortFromTheCallbackIsNotAnError(t *testing.T) {
	h, _ := newTestHandle(t, "one two three four five six seven eight")

	seen := 0
	err := streamMethod(h, "chat", `{"model":"demo","messages":[{"role":"user","content":"go"}]}`,
		func(string) error {
			seen++
			if seen == 2 {
				return errCallbackStop
			}
			return nil
		})
	if !abortedError(err) {
		t.Fatalf("stream returned %v, want an error satisfying abortedError so the shim can "+
			"translate it into a clean return", err)
	}
	if seen != 2 {
		t.Errorf("callback ran %d times, want 2 — the stream did not stop when asked", seen)
	}
}

func TestStreamRejectsAnUnroutableModel(t *testing.T) {
	h, _ := newTestHandle(t, "x")
	err := streamMethod(h, "chat", `{"model":"no-such-model","messages":[]}`,
		func(string) error { t.Error("callback ran for an unroutable model"); return nil })
	if err == nil {
		t.Fatal("stream accepted a model with no route")
	}
}

// Two handles in one process must not interfere: this is a shared library, and
// a host that builds a second gateway (different config, different provider)
// while the first is live is an ordinary thing to do.
func TestTwoHandlesAreIndependent(t *testing.T) {
	h1, fs1 := newTestHandle(t, "answer one")
	h2, fs2 := newTestHandle(t, "answer two")

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i, h := range []uint64{h1, h2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = callMethod(h, "chat",
				`{"model":"demo","messages":[{"role":"user","content":"ping"}]}`)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	if !strings.Contains(results[0], "answer one") {
		t.Errorf("handle 1 answered %s, want the first upstream's text", results[0])
	}
	if !strings.Contains(results[1], "answer two") {
		t.Errorf("handle 2 answered %s, want the second upstream's text", results[1])
	}
	if fs1.up.ChatHits() != 1 || fs2.up.ChatHits() != 1 {
		t.Errorf("upstream hits = %d and %d, want 1 each — the handles shared a gateway",
			fs1.up.ChatHits(), fs2.up.ChatHits())
	}
}

// ---------------------------------------------------------------------------

type fakeServer struct {
	up         *fake.Upstream
	configJSON string
}

func startFake(t *testing.T, text string) *fakeServer {
	t.Helper()
	up := fake.New(text)
	srv := httptest.NewServer(up.Handler())
	t.Cleanup(srv.Close)
	return &fakeServer{up: up, configJSON: fake.ConfigJSON(srv.URL)}
}

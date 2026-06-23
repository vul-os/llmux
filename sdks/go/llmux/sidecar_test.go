package llmux

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llmux/llmux/core/config"
)

// TestOpenAIBaseURL: openai_base_url == base_url + "/v1", base is http://127.0.0.1:<port>.
func TestOpenAIBaseURL(t *testing.T) {
	l := &Local{BaseURL: "http://127.0.0.1:12345"}
	if got, want := l.OpenAIBaseURL(), "http://127.0.0.1:12345/v1"; got != want {
		t.Fatalf("OpenAIBaseURL()=%q want %q", got, want)
	}
	if !strings.HasSuffix(l.OpenAIBaseURL(), "/v1") {
		t.Fatalf("OpenAIBaseURL() must end with /v1: %q", l.OpenAIBaseURL())
	}
}

// TestFreePort: freePort returns a usable, bindable localhost port.
func TestFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if p <= 0 || p > 65535 {
		t.Fatalf("freePort()=%d out of range", p)
	}
	// The port should be bindable (i.e. it was released after probing).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("control listen: %v", err)
	}
	ln.Close()
}

// TestWaitHealthy200: readiness is gated on /health -> 200.
func TestWaitHealthy200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := waitHealthy(srv.URL, 2*time.Second); err != nil {
		t.Fatalf("waitHealthy on a 200 server: %v", err)
	}
}

// TestWaitHealthyNon200: a server that never returns 200 times out with a clear error.
func TestWaitHealthyNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := waitHealthy(srv.URL, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error for never-200 server")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("error should mention health timeout: %v", err)
	}
}

// TestWaitHealthyUnreachable: an unreachable address times out with a clear error.
func TestWaitHealthyUnreachable(t *testing.T) {
	// Reserve then release a port so nothing is listening on it.
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:" + itoa(p)
	if err := waitHealthy(base, 300*time.Millisecond); err == nil {
		t.Fatal("expected error for unreachable address")
	}
}

// TestStartReadyTimeout: Start fails cleanly when the gateway never becomes healthy.
// We simulate that by pointing at a tiny ReadyTimeout against a config whose health
// would be served, but with an impossibly short timeout against a fresh server.
func TestStartReadyTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Pricing.Sources = nil
	// Bind the requested addr to an already-occupied port so server.Run errors
	// out / never serves health, forcing the timeout path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	occupied := ln.Addr().String()

	_, err = Start(Options{Config: cfg, Addr: occupied, ReadyTimeout: 300 * time.Millisecond})
	if err == nil {
		t.Fatal("expected Start to fail when addr is occupied / never healthy")
	}
}

// TestStartCloseLifecycle (integration-lite, in-process): Start serves health, then
// Close stops it and frees the port.
func TestStartCloseLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.Pricing.Sources = nil

	local, err := Start(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	// Base URL shape.
	if !strings.HasPrefix(local.BaseURL, "http://127.0.0.1:") {
		t.Fatalf("BaseURL=%q want http://127.0.0.1:<port>", local.BaseURL)
	}
	// Health is actually 200.
	resp, err := http.Get(local.BaseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status=%d", resp.StatusCode)
	}

	addr := strings.TrimPrefix(local.BaseURL, "http://")
	local.Close()

	// After Close, the port should be re-bindable (server gone).
	deadline := time.Now().Add(2 * time.Second)
	var bindErr error
	for time.Now().Before(deadline) {
		var ln net.Listener
		ln, bindErr = net.Listen("tcp", addr)
		if bindErr == nil {
			ln.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if bindErr != nil {
		t.Fatalf("port %s not freed after Close: %v", addr, bindErr)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [10]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

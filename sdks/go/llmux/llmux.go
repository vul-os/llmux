// Package llmux embeds the gateway in-process for Go programs.
//
// # New is the real one
//
//	// No listener, no port, no HTTP hop, no serialization.
//	gw, err := llmux.New(llmux.Options{})
//	defer gw.Close()
//	res, err := gw.Chat(ctx, &openai.ChatCompletionRequest{...})
//
// gw is a *gateway.Gateway, and gw.Chat returns a *gateway.Result carrying the
// per-request facts the HTTP shell has to discard: which provider actually
// served after failover, whether it was a cache hit, whether it went out on the
// account's own key, and the upstream's rate-limit headers.
//
// This package is a thin convenience over core/gateway, not a required layer.
// Importing github.com/vul-os/llmux/core/gateway and calling gateway.New(cfg)
// yourself is equally supported.
//
// # Start is a deprecated loopback shim
//
//	local, err := llmux.Start(llmux.Options{})  // Deprecated
//	defer local.Close()
//	// hand local.OpenAIBaseURL() to an OpenAI-compatible HTTP client
//
// Start runs the whole HTTP server on a loopback port inside this process. It
// costs a port, a listener and a JSON round trip per call, and it gives you
// none of the Result fields above — the HTTP tax without the process isolation
// you would get from a real sidecar. It is kept for one job: handing a base URL
// to an OpenAI-compatible client you did not write and cannot change.
//
// Start is also NOT the sidecar. The sidecar is a separate `llmux serve`
// process, which is what gets you per-tenant keys, budgets and crash isolation;
// see sdks/go/examples/sidecar.
//
// Provider keys are auto-detected from the environment (OPENAI_API_KEY, etc.)
// when Options.Config is nil. Passing your own Config reads nothing.
package llmux

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/server"
)

// Options configures the embedded gateway.
type Options struct {
	// Config is an explicit configuration. If nil, config.Default() is used
	// (auto-detecting providers from environment variables).
	Config *config.Config
	// Addr overrides the listen address. If empty, an ephemeral localhost port
	// is chosen.
	Addr string
	// ReadyTimeout bounds how long Start waits for health (default 10s).
	ReadyTimeout time.Duration
}

// Local is a running in-process gateway.
type Local struct {
	BaseURL string
	cancel  context.CancelFunc
	done    chan struct{}
}

// New builds an in-process gateway with NO listener, no loopback port and no
// HTTP hop — the real embedding path. Dispatch with gw.Chat / gw.ChatStream /
// gw.Embed and close it when done.
//
// It starts nothing: no price-catalog sync, no spend flusher, no background
// traffic at all. Call gw.Run(ctx) (or gw.Start(ctx)) if you want that work.
//
// Options.Addr and Options.ReadyTimeout are ignored — they only mean something
// for the loopback sidecar Start builds.
func New(opts Options) (*gateway.Gateway, error) {
	cfg := opts.Config
	if cfg == nil {
		// config.Default() reads the environment for provider keys. That is an
		// explicit opt-in here: passing your own Config reads nothing.
		cfg = config.Default()
	}
	return gateway.New(cfg)
}

// Start launches the gateway behind a loopback HTTP listener in a background
// goroutine and returns once it is serving (health endpoint OK).
//
// Deprecated: this is a loopback shim, not embedding — it costs a port, a
// listener and a JSON round-trip per call. Use New for in-process dispatch.
// Start remains for handing an OpenAI-compatible HTTP client a base URL.
func Start(opts Options) (*Local, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}
	addr := opts.Addr
	if addr == "" {
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		addr = fmt.Sprintf("127.0.0.1:%d", p)
	}
	cfg.Server.Addr = addr
	cfg.Server.SocketPath = ""

	srv, err := server.New(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Local{BaseURL: "http://" + addr, cancel: cancel, done: make(chan struct{})}

	go func() {
		defer close(l.done)
		_ = srv.Run(ctx)
	}()

	timeout := opts.ReadyTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if err := waitHealthy(l.BaseURL, timeout); err != nil {
		cancel()
		return nil, err
	}
	return l, nil
}

// OpenAIBaseURL returns the …/v1 base URL for OpenAI-compatible clients.
func (l *Local) OpenAIBaseURL() string { return l.BaseURL + "/v1" }

// Close shuts the gateway down and waits for it to stop.
func (l *Local) Close() {
	l.cancel()
	<-l.done
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("llmux did not become healthy within %s: %v", timeout, last)
}

// Package llmux embeds the gateway in-process for Go programs.
//
// Unlike the Python/Node packages — which spawn the binary as a local sidecar —
// Go can run the gateway directly in-process, no subprocess required:
//
//	local, err := llmux.Start(llmux.Options{})
//	defer local.Close()
//	// point any OpenAI-compatible Go client at local.OpenAIBaseURL()
//
// Provider keys are auto-detected from the environment (OPENAI_API_KEY, etc.).
package llmux

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/server"
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

// Start launches the gateway in a background goroutine and returns once it is
// serving (health endpoint OK).
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

// Command sidecar talks to `llmux serve` over HTTP, spawning and supervising
// the process itself so the operator never runs a server by hand.
//
// A Go program does not need this — see ../direct, which is strictly better in
// Go. It is here because the sidecar is the mode every other language shares,
// and because there are three real reasons a Go host still picks it:
//
//   - You want per-tenant virtual keys, budgets and model allow-lists. Those
//     are enforced by the HTTP shell's auth middleware. An in-process caller is
//     inside the trust boundary and bypasses them by construction.
//   - You want one gateway shared by several processes (or several languages)
//     with one cache, one spend ledger and one price catalog.
//   - You want llmux to be restartable, upgradable or crash-isolated
//     independently of your program.
//
// If none of those apply, use ../direct.
//
// Run it:
//
//	# against your own providers
//	go run ./sdks/go/examples/sidecar -model gpt-4o-mini
//
//	# offline, against the repo's fake upstream
//	./sdks/go/examples/run.sh sidecar
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	bin := flag.String("bin", envOr("LLMUX_BINARY", "llmux"),
		"path to the llmux binary (or a name on PATH)")
	configPath := flag.String("config", os.Getenv("LLMUX_CONFIG"),
		"path to an llmux.json for the child process")
	model := flag.String("model", "demo", "model to route")
	prompt := flag.String("prompt", "count to four", "user message")
	flag.Parse()

	sc, err := startSidecar(*bin, *configPath)
	if err != nil {
		return err
	}
	// One defer covers every path below, including the error returns. Without
	// it a failed chat leaks a serving llmux process holding a port.
	defer sc.Close()
	fmt.Println("sidecar:", sc.BaseURL, "pid", sc.cmd.Process.Pid)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ---------------------------------------------------------------- models
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := sc.getJSON(ctx, "/v1/models", &models); err != nil {
		return fmt.Errorf("models: %w", err)
	}
	ids := make([]string, 0, len(models.Data))
	for _, m := range models.Data {
		ids = append(ids, m.ID)
	}
	fmt.Println("models:", strings.Join(ids, ", "))

	// ------------------------------------------------------------------ chat
	// This is the same JSON the C ABI takes. One wire contract, two transports.
	body := map[string]any{
		"model":    *model,
		"messages": []map[string]string{{"role": "user", "content": *prompt}},
	}
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := sc.postJSON(ctx, "/v1/chat/completions", body, &chat); err != nil {
		return fmt.Errorf("chat: %w", err)
	}
	if len(chat.Choices) == 0 {
		return errors.New("chat: no choices in response")
	}
	fmt.Printf("chat:   %s\n", chat.Choices[0].Message.Content)
	if chat.Usage != nil {
		fmt.Printf("tokens: %d+%d\n", chat.Usage.PromptTokens, chat.Usage.CompletionTokens)
	}
	// Note what is NOT here: which provider served after failover, whether it
	// was a cache hit, whether it was BYOK. The HTTP shell does not put those
	// on the response object. The direct example prints all three.

	// ---------------------------------------------------------------- stream
	body["stream"] = true
	fmt.Print("stream: ")
	chunks, err := sc.postSSE(ctx, "/v1/chat/completions", body, func(chunk []byte) error {
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk, &c); err != nil {
			return err
		}
		for _, ch := range c.Choices {
			fmt.Print(ch.Delta.Content)
		}
		return nil
	})
	fmt.Println()
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	fmt.Printf("chunks: %d\n", chunks)

	return nil
}

// ---------------------------------------------------------------------------
// The supervised child process
// ---------------------------------------------------------------------------

// sidecar is a running `llmux serve` owned by this program.
type sidecar struct {
	BaseURL string
	cmd     *exec.Cmd
	client  *http.Client
}

// startSidecar picks a free port, spawns the binary on it, and waits until
// /health answers 200. On any failure it kills whatever it started, so the
// caller never has to clean up after a constructor that returned an error.
func startSidecar(bin, configPath string) (*sidecar, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "LLMUX_ADDR="+addr)
	if configPath != "" {
		cmd.Env = append(cmd.Env, "LLMUX_CONFIG="+configPath)
	}
	// The child's logs are the operator's, not ours to swallow.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}

	sc := &sidecar{
		BaseURL: "http://" + addr,
		cmd:     cmd,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	if err := sc.waitHealthy(10 * time.Second); err != nil {
		sc.Close()
		return nil, err
	}
	return sc, nil
}

// Close stops the child and reaps it. Idempotent.
func (s *sidecar) Close() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	_, _ = s.cmd.Process.Wait()
	s.cmd = nil
}

func (s *sidecar) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	var last error = errors.New("connection refused")
	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("llmux exited before becoming healthy: %s", s.cmd.ProcessState)
		}
		resp, err := client.Get(s.BaseURL + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("health returned %s", resp.Status)
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("llmux did not become healthy within %s: %w", timeout, last)
}

func (s *sidecar) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return err
	}
	return s.do(req, out)
}

func (s *sidecar) postJSON(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return s.do(req, out)
}

func (s *sidecar) do(req *http.Request, out any) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// llmux returns an OpenAI-shaped error object. Surface it verbatim
		// rather than paraphrasing a status code.
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

// postSSE streams `text/event-stream`, handing each `data:` payload to onChunk
// and stopping at the terminal [DONE] frame. Returns the chunk count.
//
// This is the sidecar's equivalent of llmux_stream, and the reason a binding
// whose FFI cannot take a C callback still has an honest streaming path.
func (s *sidecar) postSSE(ctx context.Context, path string, body any, onChunk func([]byte) error) (int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	var n int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		n++
		if err := onChunk([]byte(payload)); err != nil {
			return n, err
		}
	}
	return n, sc.Err()
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

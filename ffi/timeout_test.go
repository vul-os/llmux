package main

// Nothing on this boundary may block a host thread forever.
//
// llmux_new and llmux_stream are synchronous C calls: while one is running, the
// host's thread is inside Go and the host cannot get it back. Before this there
// were three ways to lose one permanently — an unreachable Postgres DSN
// (gateway.New used context.Background()), a streaming upstream that accepted
// the connection and said nothing (no bound of any kind on the streaming path),
// and no cancellation in the ABI at all: llmux_close was the only lever and it
// destroys the gateway.
//
// Every test here fails by TIMING OUT rather than by asserting a value, which is
// the honest shape for this defect: the bug is "it never returns".

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// silentStreamUpstream accepts POST /v1/chat/completions, optionally emits some
// chunks, and then holds the connection open forever saying nothing.
func silentStreamUpstream(t *testing.T, chunks int) string {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			b, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-x", "object": "chat.completion.chunk", "model": "upstream-model",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "w "}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	return srv.URL
}

// streamConfig is fake.ConfigJSON plus explicit liveness bounds.
func streamConfig(baseURL string, firstByte, idle int) string {
	cfg := map[string]any{
		"providers": []any{map[string]any{
			"name": "fake", "type": "passthrough", "base_url": strings.TrimRight(baseURL, "/") + "/v1",
		}},
		"routes": []any{map[string]any{
			"model": "demo", "provider": "fake", "target_model": "upstream-model",
		}},
		"stream_first_byte_timeout_seconds": firstByte,
		"stream_idle_timeout_seconds":       idle,
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

const chatReq = `{"model":"demo","messages":[{"role":"user","content":"go"}]}`

func TestNewGivesUpOnAnUnreachablePostgres(t *testing.T) {
	db := newDeadListener(t) // accepts, then never completes the handshake
	doc := fmt.Sprintf(`{"server":{"addr":":4000"},"postgres":%q,"postgres_connect_timeout_seconds":1}`, db.dsn())

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		h, err := openGateway(doc)
		if err == nil {
			closeGateway(h)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("openGateway succeeded against a DSN that never completes a handshake")
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("took %s for a 1s connect bound", took)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("llmux_new never returned. gateway.New connects and migrates Postgres with an " +
			"unbounded context, so a DSN that resolves but never answers parks the calling " +
			"thread — a HOST thread in library mode — for the life of the process.")
	}
	if n := db.conns.Load(); n == 0 {
		t.Error("nothing ever dialled the DSN, so the timeout under test was never reached and " +
			"this test proves nothing")
	}
}

func TestStreamGivesUpOnASilentUpstream(t *testing.T) {
	url := silentStreamUpstream(t, 0)
	h, err := openGateway(streamConfig(url, 1, 60))
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}
	t.Cleanup(func() { closeGateway(h) })

	done := make(chan error, 1)
	go func() { done <- streamMethod(h, "chat", chatReq, func(string) error { return nil }) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("llmux_stream returned success from an upstream that sent nothing")
		}
		if !strings.Contains(err.Error(), "stopped responding") {
			t.Errorf("error = %v, want llmux's own liveness diagnosis", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("llmux_stream never returned from an upstream that accepted the connection and " +
			"then said nothing — the host's thread is gone for good")
	}
}

// The point of llmux_cancel: a host that changes its mind mid-call gets its
// thread back without destroying the gateway.
func TestCancelUnblocksAStreamAndLeavesTheHandleUsable(t *testing.T) {
	url := silentStreamUpstream(t, 2)
	// Bounds off, so the ONLY thing that can end this stream is the cancel.
	h, err := openGateway(streamConfig(url, -1, -1))
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}
	t.Cleanup(func() { closeGateway(h) })

	chunks := make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() {
		done <- streamMethod(h, "chat", chatReq, func(string) error {
			select {
			case chunks <- struct{}{}:
			default:
			}
			return nil
		})
	}()

	// Wait until the stream is genuinely running before cancelling, so this
	// cannot pass by cancelling something that never started.
	select {
	case <-chunks:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never delivered a chunk; nothing was in flight to cancel")
	}

	cancelGateway(h)
	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled stream reported success; the host asked llmux to abandon it, " +
				"which is not the same as the stream having completed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("llmux_cancel did not unblock the stream. With no per-request cancellation the " +
			"only way out is llmux_close, which destroys the gateway.")
	}

	// And the handle is still alive on a fresh context.
	if _, err := callMethod(h, "models", ""); err != nil {
		t.Errorf("the handle is unusable after llmux_cancel: %v — cancel must abort the calls, "+
			"not the gateway", err)
	}
}

func TestCancelOnAnIdleOrUnknownHandleIsANoOp(t *testing.T) {
	h, _ := newTestHandle(t, "hello")
	cancelGateway(h)
	cancelGateway(999999) // never created
	cancelGateway(h)      // twice
	if _, err := callMethod(h, "chat", chatReq); err != nil {
		t.Errorf("a handle cancelled while idle stopped working: %v", err)
	}
}

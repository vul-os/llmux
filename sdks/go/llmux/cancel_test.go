package llmux

// This file proves the thing every non-Go SDK in sdks/ had to bind a new C
// symbol (llmux_cancel) to get: a way to abandon a blocked streaming call
// without destroying the whole gateway, that genuinely stops the upstream
// generation rather than merely returning early to the caller.
//
// Go needs no symbol. gw.Chat / gw.ChatStream already take a context.Context
// as their first argument, and core/gateway threads it, unmodified, all the
// way down to the http.Request that talks to the provider
// (core/provider/passthrough.go's post() calls http.NewRequestWithContext(ctx,
// ...), and the SSE body is read through that same request). Cancel that
// context and net/http closes the connection out from under the read loop:
// that is the entire mechanism llmux_cancel implements in C, already present
// in the standard library for anything written in Go.
//
// countingUpstream below is the in-process equivalent of sdks/fake-upstream.py
// (see that file's docstring for the full rationale). It exists so this test
// does not need a Python interpreter on the box: the property under test is
// net/http's own context propagation, and stdlib on both ends of one process
// is a stronger proof of that than shelling out to a second one. The example
// in sdks/go/examples/direct additionally runs against the real
// fake-upstream.py harness, because the numbers quoted in README.md must come
// from the same harness the other languages' READMEs cite, not from a
// same-repo lookalike.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/provider"
)

// countingUpstream is an OpenAI-compatible SSE server that sleeps before each
// streamed chunk and counts a chunk as generated only once it has actually
// been written to the response and flushed. Critically, it checks
// r.Context().Done() before every write and every sleep: once the client
// disconnects — which is exactly what a cancelled request looks like from the
// server's side of the socket — it stops producing chunks rather than running
// the completion to the end into a dead connection. Without that check this
// test would pass for the wrong reason: the consumer would stop reading at 3,
// but the upstream counter would still climb to the full count, which is
// precisely the bug fact 5 in the shared llmux_cancel brief measured on the
// FFI path (3 delivered vs. 12 generated).
type countingUpstream struct {
	words      []string
	chunkDelay time.Duration
	generated  int32 // atomic: chunks actually written to a socket
}

func (u *countingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "response writer cannot flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	write := func(payload string) bool {
		select {
		case <-r.Context().Done():
			return false
		default:
		}
		if _, err := io.WriteString(w, "data: "+payload+"\n\n"); err != nil {
			return false
		}
		fl.Flush()
		atomic.AddInt32(&u.generated, 1)
		return true
	}

	for i, word := range u.words {
		if u.chunkDelay > 0 {
			select {
			case <-time.After(u.chunkDelay):
			case <-r.Context().Done():
				return
			}
		}
		content := word
		if i != len(u.words)-1 {
			content += " "
		}
		chunk := fmt.Sprintf(`{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content)
		if !write(chunk) {
			return
		}
	}
	// Mirrors fake-upstream.py: a finish-reason chunk, then a usage chunk (the
	// gateway forces stream_options.include_usage server-side so streaming is
	// always metered), then [DONE]. Both count as "generated" the same way a
	// content chunk does, because they are equally a cost the upstream already
	// paid before a cancellation could stop it.
	if !write(`{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) {
		return
	}
	usage := fmt.Sprintf(`{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"upstream-model","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":%d,"total_tokens":%d}}`,
		len(u.words), 7+len(u.words))
	if !write(usage) {
		return
	}
	io.WriteString(w, "data: [DONE]\n\n")
	fl.Flush()
}

func newFakeGatewayConfig(baseURL string) *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{{Name: "fake", Type: config.TypePassthrough, BaseURL: baseURL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "demo", Provider: "fake"}},
	}
}

func chatRequest() *openai.ChatCompletionRequest {
	return &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("count to ten")}},
		Stream:   true,
	}
}

// TestChatStreamCancelStopsUpstream is the direct-mode proof: cancelling the
// context passed to gw.ChatStream, from inside the chunk callback, stops the
// upstream generation rather than just ending delivery to the caller.
func TestChatStreamCancelStopsUpstream(t *testing.T) {
	words := strings.Fields("one two three four five six seven eight nine ten")
	up := &countingUpstream{words: words, chunkDelay: 20 * time.Millisecond}
	srv := httptest.NewServer(up)
	defer srv.Close()

	gw, err := New(Options{Config: newFakeGatewayConfig(srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer gw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // in case the test fails before the callback ever fires

	var chunks int
	err = gw.ChatStream(ctx, chatRequest(), provider.ChunkFunc(func(c *openai.ChatCompletionChunk) error {
		chunks++
		if chunks == 3 {
			// The idiomatic Go construct IS the context.CancelFunc a caller
			// already has from context.WithCancel. Calling it here, from
			// inside the callback, is deliberate: it is the one place a
			// single-threaded consumer (an iterator's early `break`, a
			// deferred cancel from a select loop) can reach, and per fact 2
			// of the llmux_cancel brief this must be safe — no deadlock —
			// unlike closing the gateway from a callback.
			cancel()
		}
		return nil
	}))

	if chunks != 3 {
		t.Fatalf("consumer saw %d chunks, want exactly 3 (delivery should stop at the cancel point)", chunks)
	}
	if err == nil {
		t.Fatal("ChatStream returned a nil error after its context was cancelled mid-stream")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context.Canceled (or an error naming it)", err)
	}

	// Give the server goroutine a moment to observe the cancellation and stop
	// before we read its counter: the client-side error return races the
	// server noticing r.Context().Done(), and there is no signal back to the
	// test other than the counter itself settling.
	deadline := time.Now().Add(time.Second)
	var generated int32
	for time.Now().Before(deadline) {
		generated = atomic.LoadInt32(&up.generated)
		if generated >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // and confirm it does not keep climbing
	if got := atomic.LoadInt32(&up.generated); got != 3 {
		t.Fatalf("upstream generated %d chunks after a cancel at 3, want exactly 3 — "+
			"this is the bug a buffered wrapper would reintroduce: the consumer stops "+
			"reading but the upstream keeps producing and metering", got)
	}

	// The handle survives (fact 3): the same *gateway.Gateway serves a fresh
	// stream to completion after a previous stream on it was cancelled.
	var full int
	err = gw.ChatStream(context.Background(), chatRequest(), provider.ChunkFunc(func(c *openai.ChatCompletionChunk) error {
		full++
		return nil
	}))
	if err != nil {
		t.Fatalf("ChatStream on the same gateway after a cancellation: %v", err)
	}
	if want := len(words) + 2; full != want { // + the finish-reason chunk + the usage chunk
		t.Fatalf("chunks on the uncancelled run = %d, want %d", full, want)
	}
}

// TestChatStreamCancelIsPerCall documents and checks the property the FFI
// bindings must caveat in their READMEs (fact 6: llmux_cancel is per-HANDLE,
// so it aborts every call on that gateway) and Go does not have to: a
// context.Context is per-CALL. Cancelling the context of one stream must not
// touch a second call running concurrently on the same *gateway.Gateway with
// its own, uncancelled context.
func TestChatStreamCancelIsPerCall(t *testing.T) {
	words := strings.Fields("one two three four five six seven eight nine ten")
	up := &countingUpstream{words: words, chunkDelay: 20 * time.Millisecond}
	srv := httptest.NewServer(up)
	defer srv.Close()

	gw, err := New(Options{Config: newFakeGatewayConfig(srv.URL)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer gw.Close()

	// Stream A gets cancelled after 3 chunks.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() {
		var n int
		errA <- gw.ChatStream(ctxA, chatRequest(), provider.ChunkFunc(func(c *openai.ChatCompletionChunk) error {
			n++
			if n == 3 {
				cancelA()
			}
			return nil
		}))
	}()

	// Stream B runs on its own, never-cancelled context, concurrently on the
	// same gateway handle. If cancellation were per-handle (as it is in every
	// other language's binding), this would also be aborted by cancelA().
	var chunksB int
	errB := gw.ChatStream(context.Background(), chatRequest(), provider.ChunkFunc(func(c *openai.ChatCompletionChunk) error {
		chunksB++
		return nil
	}))

	if err := <-errA; err == nil {
		t.Fatal("stream A: expected an error from its own cancellation")
	}
	if errB != nil {
		t.Fatalf("stream B: unaffected call failed: %v (per-call cancellation must not cross streams)", errB)
	}
	if want := len(words) + 2; chunksB != want {
		t.Fatalf("stream B chunks = %d, want %d (it must run to completion untouched by A's cancel)", chunksB, want)
	}
}

// TestStartStreamCancelStopsUpstream is the same proof for Start/Local — the
// loopback HTTP path this package also offers ("the sidecar path in that
// package": a full server, on a port, inside this process). A Go user talking
// to local.OpenAIBaseURL() should not have to know or care that they are one
// HTTP hop further from the provider than gw.ChatStream is: cancelling the
// context on THEIR request must still reach the upstream.
//
// The chain is: this test's http.Client cancels its request context -> the
// client's Transport closes the connection to the loopback server -> the
// stdlib http.Server observes the closed connection and cancels r.Context()
// -> core/server/chat.go's streamChat passes r.Context() into
// Gateway.ChatStreamSink unchanged (see core/server/chat.go) -> the same
// dialCtx/callCtx propagation TestChatStreamCancelStopsUpstream already
// proved for direct mode. Nothing in sdks/go/llmux has to do anything extra
// for this to work; the test exists to confirm that chain has no weak link,
// not to add one.
func TestStartStreamCancelStopsUpstream(t *testing.T) {
	words := strings.Fields("one two three four five six seven eight nine ten")
	up := &countingUpstream{words: words, chunkDelay: 20 * time.Millisecond}
	srv := httptest.NewServer(up)
	defer srv.Close()

	cfg := newFakeGatewayConfig(srv.URL)
	cfg.Pricing.Sources = nil // no network in tests

	local, err := Start(Options{Config: cfg})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer local.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"model":    "demo",
		"messages": []map[string]string{{"role": "user", "content": "count to ten"}},
		"stream":   true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, local.OpenAIBaseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var chunks int
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		chunks++
		if chunks == 3 {
			// Same idiom as the direct-mode test, one layer further out: the
			// caller of an OpenAI-compatible HTTP client cancels the context
			// THEY passed to http.NewRequestWithContext. They never see a
			// gateway handle at all — which is exactly the point of Go not
			// needing llmux_cancel as a bound symbol.
			cancel()
		}
	}

	if chunks != 3 {
		t.Fatalf("consumer saw %d SSE chunks, want exactly 3", chunks)
	}

	deadline := time.Now().Add(time.Second)
	var generated int32
	for time.Now().Before(deadline) {
		generated = atomic.LoadInt32(&up.generated)
		if generated >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&up.generated); got != 3 {
		t.Fatalf("upstream generated %d chunks after the client cancelled at 3, want exactly 3", got)
	}
}

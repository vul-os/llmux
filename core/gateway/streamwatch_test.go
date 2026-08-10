package gateway

// Streaming liveness bounds.
//
// A streaming call used to have no bound of any kind: config.UpstreamTimeoutSeconds
// applies only to the unary path, and the streaming HTTP client is deliberately
// built without a client-level Timeout so long generations are not truncated.
// The consequence was that an upstream which accepted the TCP connection and
// then said nothing — a wedged inference server, a load balancer holding a
// connection open to a dead backend, a silently dropped route — blocked the
// caller forever. In the sidecar that is a leaked request goroutine; through the
// C ABI it is one of the HOST's threads, parked for the life of the process.
//
// The fix is liveness, not a deadline: time-to-first-chunk and the gap between
// chunks. TestASlowButLiveStreamIsNotTruncated is the other half of the
// evidence — a bound that also kills working streams is not a fix.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/openai"
)

// silentUpstream answers /v1/chat/completions with SSE headers, writes the
// chunks it was given (flushing each), and then holds the connection open
// saying nothing until the request context dies.
func silentUpstream(t *testing.T, chunks []string, gap time.Duration, finish bool) *httptest.Server {
	t.Helper()
	// Closed at the end of the test. net/http only cancels a handler's request
	// context from a closed connection once the request BODY has been consumed,
	// and a handler parked forever would make httptest.Server.Close hang — so the
	// handler waits on this too. It is test plumbing, not part of the scenario.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server ResponseWriter cannot flush; the chunks would arrive in one lump")
			return
		}
		for _, c := range chunks {
			select {
			case <-r.Context().Done():
				return
			case <-stop:
				return
			case <-time.After(gap):
			}
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		if finish {
			// A well-behaved upstream: terminate the stream and let the response
			// end. Only then is "the stream survived" a statement about the idle
			// bound rather than about an upstream that simply never finished.
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		// Otherwise: silence. The connection stays open and nothing more is ever
		// sent, which is the scenario — an upstream that accepted and went away.
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() {
		close(stop)
		srv.Close()
	})
	return srv
}

func chunkJSON(text string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion.chunk", "model": "upstream-model",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": text},
		}},
	})
	return string(b)
}

func streamingGateway(t *testing.T, url string, firstByte, idle int) *Gateway {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{
			Name: "fake", Type: config.TypePassthrough, BaseURL: url + "/v1", Tier: "local",
		}},
		Routes:                        []config.RouteConfig{{Model: "*", Provider: "fake", TargetModel: "upstream-model"}},
		Retry:                         config.RetryConfig{MaxRetries: 0},
		StreamFirstByteTimeoutSeconds: firstByte,
		StreamIdleTimeoutSeconds:      idle,
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// collectSink records chunks and never blocks.
type collectSink struct{ n int }

func (s *collectSink) Open() error                               { return nil }
func (s *collectSink) Chunk(c *openai.ChatCompletionChunk) error { s.n++; return nil }
func (s *collectSink) Failed(*openai.ErrorResponse)              {}
func (s *collectSink) Done()                                     {}

// runStream calls ChatStreamSink and reports how long it took, failing the test
// rather than hanging the suite if it never returns.
func runStream(t *testing.T, g *Gateway, sink StreamSink, budget time.Duration) (error, time.Duration) {
	t.Helper()
	req := &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("go")}},
	}
	raw, _ := json.Marshal(req)
	res, err := g.Prepare(context.Background(), req.Model)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	type outcome struct {
		err error
		d   time.Duration
	}
	ch := make(chan outcome, 1)
	start := time.Now()
	go func() {
		e := g.ChatStreamSink(context.Background(), req, raw, res, sink)
		ch <- outcome{e, time.Since(start)}
	}()
	select {
	case o := <-ch:
		return o.err, o.d
	case <-time.After(budget):
		t.Fatalf("ChatStreamSink did not return within %s. The upstream accepted the connection "+
			"and then said nothing; with no liveness bound this call never returns and the "+
			"caller's thread is gone for the life of the process.", budget)
		return nil, 0
	}
}

func TestStreamGivesUpWhenTheFirstChunkNeverArrives(t *testing.T) {
	srv := silentUpstream(t, nil, 0, false)
	g := streamingGateway(t, srv.URL, 1, 60)

	sink := &collectSink{}
	err, took := runStream(t, g, sink, 15*time.Second)
	if err == nil {
		t.Fatal("ChatStreamSink returned nil for a stream that produced nothing")
	}
	if !errors.Is(err, ErrStreamTimeout) {
		t.Errorf("error = %v, want one wrapping ErrStreamTimeout so a caller can tell llmux's "+
			"own bound from an upstream error or the caller's own cancellation", err)
	}
	if took > 5*time.Second {
		t.Errorf("took %s for a 1s first-chunk bound", took)
	}
	if sink.n != 0 {
		t.Errorf("sink saw %d chunks from an upstream that sent none", sink.n)
	}
}

func TestStreamGivesUpWhenTheChunksStop(t *testing.T) {
	srv := silentUpstream(t, []string{chunkJSON("one"), chunkJSON(" two")}, 0, false)
	g := streamingGateway(t, srv.URL, 30, 1)

	sink := &collectSink{}
	err, took := runStream(t, g, sink, 15*time.Second)
	if err == nil {
		t.Fatal("ChatStreamSink returned nil for a stream that stopped mid-flight")
	}
	if !errors.Is(err, ErrStreamTimeout) {
		t.Errorf("error = %v, want one wrapping ErrStreamTimeout", err)
	}
	if took > 6*time.Second {
		t.Errorf("took %s for a 1s idle bound", took)
	}
	if sink.n < 2 {
		t.Errorf("sink saw %d chunks, want the 2 the upstream did send before going quiet — "+
			"the bound fired too early to be an IDLE bound", sink.n)
	}
}

// The bound must be liveness, not a deadline. This stream runs for well over
// its own idle timeout in total, one chunk at a time, and must survive: a
// wall-clock timeout large enough not to truncate real generations is too large
// to catch anything, which is precisely why there wasn't one.
func TestASlowButLiveStreamIsNotTruncated(t *testing.T) {
	chunks := make([]string, 8)
	for i := range chunks {
		chunks[i] = chunkJSON("w ")
	}
	srv := silentUpstream(t, chunks, 150*time.Millisecond, true) // ~1.2s total
	g := streamingGateway(t, srv.URL, 5, 1)                      // idle bound 1s < total elapsed

	sink := &collectSink{}
	err, _ := runStream(t, g, sink, 20*time.Second)
	if errors.Is(err, ErrStreamTimeout) {
		t.Fatalf("a stream that produced a chunk every 150ms for longer than its 1s idle bound "+
			"was cut off: %v. The clock must restart on every chunk, or a legitimate long "+
			"generation gets truncated.", err)
	}
	if sink.n != len(chunks) {
		t.Errorf("sink saw %d of %d chunks", sink.n, len(chunks))
	}
}

// Turning the bounds off restores the historical behaviour exactly, for anyone
// who needs it. Asserting the call is STILL blocked after a wait proves the
// negative value reached the watchdog rather than being read as "0 = default".
func TestNegativeTimeoutsDisableTheBounds(t *testing.T) {
	srv := silentUpstream(t, nil, 0, false)
	g := streamingGateway(t, srv.URL, -1, -1)

	req := &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("go")}},
	}
	raw, _ := json.Marshal(req)
	res, err := g.Prepare(context.Background(), req.Model)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.ChatStreamSink(ctx, req, raw, res, &collectSink{}) }()

	select {
	case e := <-done:
		t.Fatalf("the stream ended (%v) although both bounds were disabled", e)
	case <-time.After(2 * time.Second):
	}
	cancel() // and the caller's own context still works
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the caller's context did not end the stream")
	}
}

func TestTimeoutResolutionConventions(t *testing.T) {
	c := &config.Config{}
	if got := c.StreamFirstByteTimeout(); got != config.DefaultStreamFirstByteTimeoutSeconds*time.Second {
		t.Errorf("zero value first-byte timeout = %s, want the built-in default: a hand-built "+
			"Config must be bounded too, not inherit 'forever' from a zero int", got)
	}
	if got := c.StreamIdleTimeout(); got != config.DefaultStreamIdleTimeoutSeconds*time.Second {
		t.Errorf("zero value idle timeout = %s, want the built-in default", got)
	}
	if got := c.PostgresConnectTimeout(); got != config.DefaultPostgresConnectTimeoutSeconds*time.Second {
		t.Errorf("zero value postgres connect timeout = %s, want the built-in default", got)
	}
	off := &config.Config{
		StreamFirstByteTimeoutSeconds: -1,
		StreamIdleTimeoutSeconds:      -1,
		PostgresConnectTimeoutSeconds: -1,
	}
	if off.StreamFirstByteTimeout() != 0 || off.StreamIdleTimeout() != 0 || off.PostgresConnectTimeout() != 0 {
		t.Error("a negative value must mean 'no bound'; it is the documented opt-out")
	}
	set := &config.Config{StreamIdleTimeoutSeconds: 7}
	if set.StreamIdleTimeout() != 7*time.Second {
		t.Errorf("configured idle timeout = %s, want 7s", set.StreamIdleTimeout())
	}
}

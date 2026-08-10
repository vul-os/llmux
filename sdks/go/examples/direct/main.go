// Command direct embeds llmux in this process — no port, no listener, no
// second process, and no shared library.
//
// This is the whole Go story: llmux is a Go program, so a Go host imports it.
// `sdks/go/llmux` is a thin convenience over `core/gateway`; you can skip it
// and call gateway.New yourself. Either way you get *gateway.Gateway, and the
// per-request facts the HTTP shell has to flatten away — which provider
// actually served after failover, whether it was a cache hit, whether it went
// out on the account's own key — are right there on the Result.
//
// Every other language reaches this same object through the C ABI in ffi/,
// which costs a 12 MB shared library, the Go runtime in the host process, and
// a JSON round trip on every call. Go pays none of that. When you read the
// Rust or Swift example next to this one, that is the difference you are
// looking at.
//
// Run it:
//
//	# against your own providers (keys come from the environment)
//	go run ./sdks/go/examples/direct -model gpt-4o-mini -prompt 'say hi'
//
//	# offline, against the repo's fake upstream (no keys, deterministic)
//	./sdks/go/examples/run.sh direct
//
//	# cancellation, against sdks/fake-upstream.py (see -cancel-demo below)
//	./sdks/go/examples/run.sh direct
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/provider"
	"github.com/vul-os/llmux/sdks/go/llmux"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFlag := flag.String("config", os.Getenv("LLMUX_CONFIG_JSON"),
		"llmux configuration: a JSON document, or @path to read one from a file. "+
			"Empty means config.Default() — built-in defaults plus provider keys "+
			"auto-detected from the environment.")
	model := flag.String("model", "demo", "model to route")
	prompt := flag.String("prompt", "count to four", "user message")
	cancelDemo := flag.Bool("cancel-demo", false,
		"run the context-cancellation walkthrough instead of the standard one "+
			"(point -config at sdks/fake-upstream.py's CONFIG line, run with "+
			"--chunk-delay-ms so there is something to cancel in the middle of)")
	flag.Parse()

	cfg, err := loadConfig(*cfgFlag)
	if err != nil {
		return err
	}

	// New builds the gateway with NO listener. It starts no goroutines: there
	// is no price-catalog sync and no spend flusher unless you ask for one with
	// gw.Run(ctx). Constructing it is inert.
	gw, err := llmux.New(llmux.Options{Config: cfg})
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}
	// The handle-safety construct in Go is defer. It runs on the error paths
	// below too, which is the entire point of putting it here rather than at
	// the end of the happy path.
	defer func() {
		if cerr := gw.Close(); cerr != nil {
			fmt.Fprintln(os.Stderr, "warning: close:", cerr)
		}
	}()

	if *cancelDemo {
		return runCancelDemo(gw, cfg, *model)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ---------------------------------------------------------------- models
	// Answered from memory. No upstream is contacted, which is why this is the
	// row the ffi/ benchmark uses to measure the boundary itself.
	fmt.Println("models:", strings.Join(gw.Models(), ", "))

	// ------------------------------------------------------------------ chat
	res, err := gw.Chat(ctx, &openai.ChatCompletionRequest{
		Model:    *model,
		Messages: []openai.Message{{Role: "user", Content: openai.Str(*prompt)}},
	})
	if err != nil {
		return fmt.Errorf("chat: %w", err)
	}
	if len(res.Response.Choices) == 0 {
		return errors.New("chat: no choices in response")
	}
	fmt.Printf("chat:   %s\n", res.Response.Choices[0].Message.Content.String())

	// These four facts exist only in-process. Over HTTP the shell serializes
	// the OpenAI response and they are gone.
	fmt.Printf("served: provider=%s cache_hit=%v byok=%v", res.Provider, res.CacheHit, res.BYOK)
	if u := res.Response.Usage; u != nil {
		fmt.Printf(" tokens=%d+%d", u.PromptTokens, u.CompletionTokens)
	}
	fmt.Println()

	// ---------------------------------------------------------------- stream
	// The callback form. It returns when the stream ends; returning a non-nil
	// error from the callback stops it and surfaces here.
	fmt.Print("stream: ")
	var chunks int
	err = gw.ChatStream(ctx, &openai.ChatCompletionRequest{
		Model:    *model,
		Messages: []openai.Message{{Role: "user", Content: openai.Str(*prompt)}},
		Stream:   true,
	}, provider.ChunkFunc(func(c *openai.ChatCompletionChunk) error {
		chunks++
		for _, ch := range c.Choices {
			fmt.Print(ch.Delta.Content)
		}
		return nil
	}))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("stream: %w", err)
	}
	fmt.Printf("chunks: %d\n", chunks)

	return nil
}

// runCancelDemo is the Go-native equivalent of what every other llmux SDK
// binds llmux_cancel to get: a way to walk away from a blocked streaming call
// that genuinely stops the upstream generation, not just the delivery to this
// process.
//
// Go never had a gap here to close. gw.ChatStream already takes a
// context.Context as its first argument, and core/gateway threads it,
// unmodified, down to the http.Request the passthrough provider makes
// (core/provider/passthrough.go calls http.NewRequestWithContext(ctx, ...)
// and reads the SSE body through that same request). Cancel the context and
// net/http closes the connection out from under the read loop. There is no
// symbol to bind because there is nothing missing: the standard library
// already does, for free, what llmux_cancel does in C.
//
// This is proved by test in sdks/go/llmux/cancel_test.go against an in-process
// counting fake. This demo runs the same shape against the real
// sdks/fake-upstream.py harness so the numbers in README.md come from the same
// tool every other language's README cites, not a same-repo lookalike.
func runCancelDemo(gw *gateway.Gateway, cfg *config.Config, model string) error {
	if len(cfg.Providers) == 0 {
		return errors.New("cancel-demo: -config has no providers; point it at " +
			"sdks/fake-upstream.py's CONFIG line")
	}
	// The fake upstream's own HTTP server serves GET /generated alongside
	// /v1/chat/completions. Its base_url is that server's address plus "/v1";
	// strip the suffix to reach /generated on the same server.
	upstreamBase := strings.TrimSuffix(cfg.Providers[0].BaseURL, "/v1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // in case we return before the callback ever fires

	fmt.Print("stream: ")
	var chunks int
	err := gw.ChatStream(ctx, &openai.ChatCompletionRequest{
		Model:    model,
		Messages: []openai.Message{{Role: "user", Content: openai.Str("count to ten")}},
		Stream:   true,
	}, provider.ChunkFunc(func(c *openai.ChatCompletionChunk) error {
		chunks++
		for _, ch := range c.Choices {
			fmt.Print(ch.Delta.Content)
		}
		if chunks == 3 {
			// The idiomatic construct IS this context.CancelFunc. Calling it
			// from inside the callback (rather than from another goroutine)
			// is deliberate: it is the one place a single-threaded consumer
			// can reach, and it must be safe to do so — no deadlock, unlike
			// closing the gateway from inside a callback (see gw.Close's
			// docs, and llmux_close's in ffi/include/llmux.h).
			cancel()
		}
		return nil
	}))
	fmt.Println()

	fmt.Printf("consumer chunks: %d\n", chunks)
	if err != nil {
		// This is the expected, successful outcome of a cancellation: the
		// call reports the failure rather than swallowing it, because a
		// cancelled stream that had already delivered chunks is a call that
		// did not complete, and tokens already served are still metered.
		fmt.Printf("stream error: %v\n", err)
	} else {
		fmt.Println("warning: stream returned no error; cancellation may not have reached it in time")
	}

	// How many chunks the upstream ACTUALLY produced is the number that
	// matters — a cancellation that returns promptly here while the provider
	// runs to completion behind it would look identical from every line
	// printed above — and the harness serves it at GET /generated.
	//
	// This program does not read it. It prints the address instead and lets
	// run.sh curl it, because this example dials NOTHING: it embeds the
	// gateway rather than talking to one, and that is the contrast the
	// direct/sidecar pair exists to draw. core/sovereign's egress guard
	// enforces exactly that — every outbound dialer in the tree must be
	// declared, and the declaration covering the sidecar example cites this
	// file's silence as the reason it is safe. Fetching a counter would have
	// been a harmless request that quietly cost the pair its meaning.
	fmt.Printf("upstream counter: %s/generated\n", upstreamBase)

	// One gateway, one cancellation scope: unlike llmux_cancel in the C ABI,
	// which is per-HANDLE and would have aborted every other call in flight on
	// the same gateway, this context was per-CALL. The gateway is untouched, so
	// it still answers.
	//
	// Deliberately `models` and not a second stream. A second stream would run
	// to completion through the same upstream and add its twelve chunks to the
	// counter run.sh is about to read, turning the measured "3" into a "15"
	// that means nothing without a subtraction — a demo that quietly spoils its
	// own measurement to make a second point. That point is made properly by
	// TestChatStreamCancelIsPerCall in sdks/go/llmux/cancel_test.go, which owns
	// its own counter and can afford it.
	fmt.Printf("handle after cancel: models -> %s\n", strings.Join(gw.Models(), ", "))

	return nil
}

// loadConfig turns the -config flag into a *config.Config. An empty flag means
// config.Default(), which reads OPENAI_API_KEY and friends from the
// environment; passing your own document reads nothing.
func loadConfig(spec string) (*config.Config, error) {
	switch {
	case spec == "":
		return config.Default(), nil
	case strings.HasPrefix(spec, "@"):
		return config.Load(strings.TrimPrefix(spec, "@"))
	default:
		if !json.Valid([]byte(spec)) {
			return nil, fmt.Errorf("-config is neither @path nor valid JSON")
		}
		return config.FromJSON([]byte(spec))
	}
}

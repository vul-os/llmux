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

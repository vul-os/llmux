// Command libonly is a host program that embeds llmux as a LIBRARY and nothing
// else: it imports core/gateway and core/config, and never core/server or web.
//
// It exists to be BUILT, not run. embedtest's G7 guard compiles it and then
// scans the resulting binary for bytes that could only have come from the
// admin console's go:embed. An embedder who wants an in-process LLM gateway
// must not be made to ship 21 KB of admin HTML they can never serve.
package main

import (
	"context"
	"fmt"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
)

func main() {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "local", Type: config.TypePassthrough, BaseURL: "http://127.0.0.1:11434/v1",
		}},
		Routes: []config.RouteConfig{{Model: "*", Provider: "local"}},
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		panic(err)
	}
	defer gw.Close()

	// Reference the dispatch surface so the linker cannot drop the library as
	// dead code and make the "no UI bytes" assertion trivially true.
	_, _ = gw.Chat(context.Background(), &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("ping")}},
	})
	fmt.Println(len(gw.Models()))
}

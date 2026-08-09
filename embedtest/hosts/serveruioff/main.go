// Command serveruioff mounts the HTTP shell with the console switched OFF at
// runtime (server.Options{UI: false}).
//
// It exists to pin down where the real boundary is. The UI FLAG is not it:
// mountUI only decides whether the route is registered, while core/server
// still references webui.HTML() from a branch the linker cannot prune, so the
// console's bytes ship either way. The boundary is the IMPORT — link
// core/server and you carry the console; link only core/gateway and you do
// not. embedtest's G7 asserts both halves so nobody documents the flag as a
// size lever it is not.
package main

import (
	"fmt"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/server"
)

func main() {
	cfg := &config.Config{Providers: []config.ProviderConfig{{Name: "local", Type: config.TypePassthrough, BaseURL: "http://127.0.0.1:11434/v1"}}}
	srv, err := server.NewWithOptions(cfg, server.Options{UI: false})
	if err != nil {
		panic(err)
	}
	fmt.Println(srv.Handler() != nil)
}

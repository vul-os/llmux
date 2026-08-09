// Command withui is the POSITIVE CONTROL for the libonly host: same library,
// but mounted behind the HTTP shell with the admin console enabled. Its binary
// must contain the console's bytes.
//
// Without this control, "libonly contains no UI bytes" would be equally true if
// the sentinel strings were misspelled, or if go:embed data were somehow not
// searchable in a linked binary — the guard would be green forever while
// checking nothing.
package main

import (
	"fmt"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/server"
)

func main() {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "local", Type: config.TypePassthrough, BaseURL: "http://127.0.0.1:11434/v1",
		}},
		Routes: []config.RouteConfig{{Model: "*", Provider: "local"}},
	}
	srv, err := server.NewWithOptions(cfg, server.Options{UI: true})
	if err != nil {
		panic(err)
	}
	fmt.Println(srv.Handler() != nil)
}

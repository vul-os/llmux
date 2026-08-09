// Command fakeupstream serves the OpenAI-compatible fake that the C smoke test
// and the latency benchmark point llmux at.
//
// It prints three lines to stdout and then serves until killed:
//
//	URL <base url>
//	CONFIG <llmux configuration JSON routing "demo" at that URL>
//	TEXT <the assistant text every answer carries>
//
// Printing the CONFIG rather than letting each caller compose one is the point:
// the C test would otherwise hand-roll the same JSON in a string literal, and
// the day the config schema changes, the Go tests and the C test would disagree
// about what they are testing.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/vul-os/llmux-ffi/internal/fake"
)

func main() {
	text := flag.String("text", "one two three four", "assistant text to answer with")
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	base := "http://" + ln.Addr().String()

	up := fake.New(*text)
	fmt.Printf("URL %s\n", base)
	fmt.Printf("CONFIG %s\n", fake.ConfigJSON(base))
	fmt.Printf("TEXT %s\n", *text)
	if err := os.Stdout.Sync(); err != nil {
		// Stdout may be a pipe the runner is reading line by line; a failure to
		// flush would hang it, so say so rather than serving invisibly.
		log.Printf("flush stdout: %v", err)
	}

	srv := &http.Server{Handler: up.Handler()}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}

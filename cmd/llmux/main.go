// Command llmux is the gateway binary. It runs both as a standalone server and,
// in local sidecar mode, as the process auto-spawned by per-language packages.
//
// It is a subcommand CLI. With no subcommand (or "serve"), it runs the server,
// so existing usage `llmux -config x.json` keeps working.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/server"
)

// version is the binary version string.
var version = "0.1.0"

func main() {
	// Determine the subcommand. Default to "serve" so bare-flag invocations
	// (e.g. `llmux -config x.json`) keep working unchanged.
	sub := "serve"
	args := os.Args[1:]
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "serve":
		runServe(args)
	case "version":
		fmt.Println(version)
	case "models":
		runModels(args)
	case "catalog":
		runCatalog(args)
	case "keys":
		runKeys(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "llmux: unknown subcommand %q\n\n", sub)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `llmux — OpenAI-compatible LLM gateway

Usage:
  llmux [serve] [-config FILE]   run the gateway server (default)
  llmux version                  print the version
  llmux models [--addr URL]      list models with pricing and context window
  llmux catalog [--addr URL]     show price catalog count and update time
  llmux keys [--addr URL] [--key KEY]
                                 list virtual keys (budget, spend, rpm)
  llmux help                     show this help

Default --addr is `+defaultAddr+`.
`)
}

// runServe runs the gateway server (the original main behavior).
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("LLMUX_CONFIG"), "path to JSON config file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("llmux: config error: %v", err)
	}
	log.Printf("llmux: %s", cfg)

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("llmux: init error: %v", err)
	}

	// Optional JSONL usage log for billing/analytics.
	if path := os.Getenv("LLMUX_USAGE_LOG"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatalf("llmux: cannot open usage log %s: %v", path, err)
		}
		defer f.Close()
		srv.SetUsageLogger(server.NewJSONLUsageLogger(f))
		log.Printf("llmux: usage log -> %s", path)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("llmux: %v", err)
	}
}

func runModels(args []string) {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "gateway base URL")
	_ = fs.Parse(args)
	if err := fetchModels(*addr, os.Stdout); err != nil {
		log.Fatalf("llmux: %v", err)
	}
}

func runCatalog(args []string) {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "gateway base URL")
	_ = fs.Parse(args)
	if err := fetchCatalog(*addr, os.Stdout); err != nil {
		log.Fatalf("llmux: %v", err)
	}
}

func runKeys(args []string) {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "gateway base URL")
	key := fs.String("key", "", "master key for Authorization header")
	_ = fs.Parse(args)
	if err := fetchKeys(*addr, *key, os.Stdout); err != nil {
		log.Fatalf("llmux: %v", err)
	}
}

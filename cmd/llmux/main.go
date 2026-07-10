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
	"time"

	"github.com/llmux/llmux/core/byok"
	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/server"
	"github.com/llmux/llmux/integration/cp"
)

// Version is the binary version string; set at build time via
// -ldflags "-X main.Version=x.y.z".
var Version = "dev"

func main() {
	// Determine the subcommand. Default to "serve" so bare-flag invocations
	// (e.g. `llmux -config x.json`) keep working unchanged.
	sub := "serve"
	args := os.Args[1:]

	// Handle --version before subcommand detection (starts with '-', so it
	// would otherwise fall through to runServe and be rejected as unknown flag).
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println(Version)
		return
	}

	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "serve":
		runServe(args)
	case "version":
		fmt.Println(Version)
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

	// OPTIONAL BYOK store. When a KEK is configured, accounts can register their
	// OWN provider keys (encrypted at rest); requests for those providers then use
	// the account's key and are NOT metered/billed. With no KEK, BYOK is off and
	// every request uses the central provider keys (the standalone default).
	if kekStr := cfg.BYOK.ResolveKEK(); kekStr != "" {
		kek, err := byok.ParseKEK(kekStr)
		if err != nil {
			log.Fatalf("llmux: byok KEK: %v", err)
		}
		crypter, err := byok.NewCrypter(kek)
		if err != nil {
			log.Fatalf("llmux: byok crypter: %v", err)
		}
		var store server.BYOKStore
		if cfg.BYOK.StorePath != "" {
			fs, err := byok.NewFileStore(crypter, cfg.BYOK.StorePath)
			if err != nil {
				log.Fatalf("llmux: byok store: %v", err)
			}
			store = fs
			log.Printf("llmux: BYOK enabled (encrypted store -> %s)", cfg.BYOK.StorePath)
		} else {
			store = byok.NewMemStore(crypter)
			log.Printf("llmux: BYOK enabled (in-memory store)")
		}
		srv.SetBYOKStore(store)

		// GUARDRAIL: warn loudly when the /admin/byok/* endpoints are exposed
		// without authentication (BYOK on, no master key, TCP bind).
		if msg := byokGuardrailWarning(cfg); msg != "" {
			log.Print(msg)
		}
	}

	// Build the usage logger(s). JSONL logging (if configured) is preserved and
	// the optional cp sink composes on top of it — never replaces it.
	// UsageLogPath is resolved (later wins) from config file `usage_log_path`
	// or env LLMUX_USAGE_LOG — both bind now; previously only the env var did.
	var loggers []server.UsageLogger
	if path := cfg.UsageLogPath; path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatalf("llmux: cannot open usage log %s: %v", path, err)
		}
		defer f.Close()
		loggers = append(loggers, server.NewJSONLUsageLogger(f))
		log.Printf("llmux: usage log -> %s", path)
	}

	// OPTIONAL control-plane (cp) integration. Wired ONLY here, ONLY when a cp
	// URL is configured (config file `cp.cp_url` or env LLMUX_CP_URL). The core
	// gateway never imports integration/cp; with cp unset the gateway stays
	// fully standalone (static keys, no network calls to cp).
	cpCfg := cp.New(cfg.CP.URL, cfg.CP.SharedSecret).
		WithRPM(cfg.CP.RPM).
		WithDegradedFailOpen(cfg.CP.DegradedFailOpen).
		WithDegradedRPM(cfg.CP.DegradedRPM).
		WithUsageSpoolPath(cfg.CP.UsageSpoolPath)
	if cfg.CP.EntitlementTTLSeconds > 0 {
		cpCfg = cpCfg.WithEntitlementTTL(time.Duration(cfg.CP.EntitlementTTLSeconds) * time.Second)
	}
	if cpCfg.Enabled() {
		srv.SetIdentity(cp.NewIdentity(cpCfg))
		srv.SetBudgetGate(cp.NewBudgetGate(cpCfg))
		loggers = append(loggers, cp.NewUsageLogger(cpCfg))
		log.Printf("llmux: control plane integration on (%s)", cpCfg.BaseURL)
	}

	switch len(loggers) {
	case 0:
		// keep the server's default NopUsageLogger
	case 1:
		srv.SetUsageLogger(loggers[0])
	default:
		srv.SetUsageLogger(cp.NewMultiUsageLogger(loggers...))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("llmux: %v", err)
	}
}

// byokGuardrailWarning returns a startup warning when the /admin/byok/*
// endpoints — which register/clear an account's own provider keys — are reachable
// WITHOUT authentication. They are master-key gated, so the dangerous case is:
// BYOK enabled, no master key, and a TCP listener (anyone who can connect can
// write/list BYOK credentials). A unix socket is filesystem-permission gated
// (0600, owner-only), so a socket-only bind is not warned about. Returns "" when
// the configuration is safe.
func byokGuardrailWarning(cfg *config.Config) string {
	if cfg.BYOK.ResolveKEK() == "" {
		return "" // BYOK disabled
	}
	if cfg.Server.MasterKey != "" {
		return "" // admin endpoints are authenticated
	}
	if cfg.Server.Addr == "" {
		return "" // no TCP bind (unix socket is owner-only)
	}
	return fmt.Sprintf("llmux: WARNING: BYOK is enabled but no master key is set and the gateway is bound to a TCP socket (%s) — the /admin/byok/* endpoints are UNAUTHENTICATED and reachable by anyone who can connect. Set server.master_key (or LLMUX_MASTER_KEY) to protect them.", cfg.Server.Addr)
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

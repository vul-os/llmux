package main

import (
	"strings"
	"testing"

	"github.com/vul-os/llmux/core/config"
)

// TestBYOKGuardrailWarning verifies the unauthenticated-/admin/byok warning fires
// exactly when BYOK is enabled, no master key is set, and the gateway is bound to
// a TCP socket — and stays silent in every safe configuration.
func TestBYOKGuardrailWarning(t *testing.T) {
	const kek = "0000000000000000000000000000000000000000000000000000000000000000"
	cases := []struct {
		name string
		cfg  *config.Config
		warn bool
	}{
		{
			name: "byok+no-master+tcp warns",
			cfg:  &config.Config{BYOK: config.BYOKConfig{KEK: kek}, Server: config.ServerConfig{Addr: ":4000"}},
			warn: true,
		},
		{
			name: "byok+master+tcp safe",
			cfg:  &config.Config{BYOK: config.BYOKConfig{KEK: kek}, Server: config.ServerConfig{Addr: ":4000", MasterKey: "mk"}},
			warn: false,
		},
		{
			name: "byok+no-master+unix-only safe",
			cfg:  &config.Config{BYOK: config.BYOKConfig{KEK: kek}, Server: config.ServerConfig{SocketPath: "/tmp/llmux.sock"}},
			warn: false,
		},
		{
			name: "byok-disabled safe",
			cfg:  &config.Config{Server: config.ServerConfig{Addr: ":4000"}},
			warn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := byokGuardrailWarning(tc.cfg)
			if tc.warn {
				if msg == "" {
					t.Fatal("expected a guardrail warning, got none")
				}
				if !strings.Contains(msg, "UNAUTHENTICATED") || !strings.Contains(msg, ":4000") {
					t.Fatalf("warning missing expected detail: %q", msg)
				}
			} else if msg != "" {
				t.Fatalf("expected no warning, got %q", msg)
			}
		})
	}
}

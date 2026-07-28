package config

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sovereignty gate governs INFERENCE dispatch. The price-catalog sync is
// the one piece of off-box traffic a stock llmux makes on its own, and it does
// not pass the gate. These tests pin that fact down in both directions so it
// can neither be forgotten nor quietly become untrue:
//
//   - the default really does ship off-box feed URLs (no pretending otherwise);
//   - an operator really can switch them off in the config file.

// writeConfig writes a config file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llmux.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestDefaultPricingSourcesAreOffBoxEgress states the honest default: llmux
// ships with public price feeds configured, so a stock gateway DOES make an
// outbound request at startup. This test exists so that fact stays documented
// in code — if the default ever changes, the change is deliberate and visible
// here rather than a silent shift in what "no default network calls" means.
func TestDefaultPricingSourcesAreOffBoxEgress(t *testing.T) {
	clearKnownProviderEnv(t)
	c := Default()

	if len(c.Pricing.Sources) == 0 {
		t.Fatal("expected the shipped default to declare price feeds; if this default was intentionally " +
			"removed, update core/sovereign/egress_guard_test.go and docs/architecture.md, which both " +
			"disclose the price sync as default-on egress")
	}
	for _, s := range c.Pricing.Sources {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("pricing source %q is not a URL: %v", s, err)
		}
		host := u.Hostname()
		if host == "" {
			t.Fatalf("pricing source %q has no host", s)
		}
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			continue
		}
		// Off-box: that is the point of this assertion, not a failure.
		t.Logf("default pricing source is OFF-BOX egress (ungated by design, no prompt content): %s", s)
	}
}

// TestPricingSourcesCanBeDisabled is the operator's off switch: "sources": []
// in the config file must clear the shipped defaults, so a gateway can be made
// to open no outbound connection of its own accord. A len()>0 merge check would
// silently ignore the empty list and keep dialing the default feeds — which is
// exactly the bug this pins.
func TestPricingSourcesCanBeDisabled(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	path := writeConfig(t, `{"pricing": {"sources": []}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Pricing.Sources) != 0 {
		t.Fatalf(`"sources": [] must disable the price feeds; got %v — a stock gateway would still egress `+
			`to those hosts at startup with no way for the operator to stop it`, c.Pricing.Sources)
	}
}

// TestPricingSourcesOmittedKeepsDefaults is the other half: a config file that
// says nothing about pricing must keep the defaults. Disabling the feeds has to
// be an explicit act, not a side effect of writing any config file at all.
func TestPricingSourcesOmittedKeepsDefaults(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	path := writeConfig(t, `{"server": {"addr": ":4001"}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Pricing.Sources) != len(Default().Pricing.Sources) {
		t.Fatalf("omitting pricing must keep the defaults; got %v", c.Pricing.Sources)
	}
}

// TestPricingSourcesExplicitListReplacesDefaults proves an operator can point
// the catalog at their own mirror (e.g. an on-box file server) instead of the
// public feeds — the sovereign way to keep prices fresh without leaving the box.
func TestPricingSourcesExplicitListReplacesDefaults(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	path := writeConfig(t, `{"pricing": {"sources": ["http://localhost:9999/model_prices.json"]}}`)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Pricing.Sources) != 1 || c.Pricing.Sources[0] != "http://localhost:9999/model_prices.json" {
		t.Fatalf("explicit sources must replace the defaults; got %v", c.Pricing.Sources)
	}
}

// Package providers wires provider configs into concrete adapter instances.
// It is the single place that imports every adapter, keeping the core provider
// package free of import cycles.
package providers

import (
	"log"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/provider"
	"github.com/llmux/llmux/core/provider/anthropic"
	"github.com/llmux/llmux/core/provider/azure"
	"github.com/llmux/llmux/core/provider/bedrock"
	"github.com/llmux/llmux/core/provider/cohere"
	"github.com/llmux/llmux/core/provider/gemini"
	"github.com/llmux/llmux/core/provider/passthrough"
)

// Stability reflects how thoroughly an adapter is verified against the real
// provider API. Honest by design: only live-verified adapters are "stable".
//   - stable:       forwards/translates with high confidence (live-checked or near-verbatim passthrough)
//   - beta:         real translation, unit-tested vs mocks, NOT yet live-verified
//   - experimental: written to documented spec, unverified — use with caution
func Stability(t config.ProviderType) string {
	switch t {
	case config.TypePassthrough:
		return "stable"
	case config.TypeAnthropic, config.TypeGemini, config.TypeAzure:
		return "beta"
	case config.TypeCohere, config.TypeBedrock:
		return "experimental"
	default:
		return "unknown"
	}
}

// Build constructs a provider registry from the given configs. Providers whose
// type has no adapter yet are skipped with a warning rather than failing the
// whole gateway. Experimental/beta adapters are logged so operators know what
// is not yet live-verified.
func Build(cfgs []config.ProviderConfig) (*provider.Registry, error) {
	reg := provider.NewRegistry()
	for _, c := range cfgs {
		var p provider.Provider
		switch c.Type {
		case config.TypePassthrough:
			p = passthrough.New(c)
		case config.TypeAnthropic:
			p = anthropic.New(c)
		case config.TypeGemini:
			p = gemini.New(c)
		case config.TypeCohere:
			p = cohere.New(c)
		case config.TypeBedrock:
			p = bedrock.New(c)
		case config.TypeAzure:
			p = azure.New(c)
		default:
			log.Printf("llmux: skipping provider %q: no adapter for type %q yet", c.Name, c.Type)
			continue
		}
		if err := reg.Register(p); err != nil {
			return nil, err
		}
		if s := Stability(c.Type); s != "stable" {
			log.Printf("llmux: provider %q (%s) is %s — not yet verified against the live API", c.Name, c.Type, s)
		}
	}
	return reg, nil
}

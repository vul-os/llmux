// Package providers wires provider configs into concrete adapter instances.
// It is the single place that imports every adapter, keeping the core provider
// package free of import cycles.
package providers

import (
	"log/slog"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/provider"
	"github.com/vul-os/llmux/core/provider/anthropic"
	"github.com/vul-os/llmux/core/provider/azure"
	"github.com/vul-os/llmux/core/provider/bedrock"
	"github.com/vul-os/llmux/core/provider/cohere"
	"github.com/vul-os/llmux/core/provider/gemini"
	"github.com/vul-os/llmux/core/provider/passthrough"
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
//
// opts are the per-gateway adapter options (response-size cap, drop_params, HTTP
// clients). They are passed to every adapter rather than set as package globals,
// so two gateways in one process never share or overwrite each other's settings.
// log may be nil (nothing is logged).
func Build(cfgs []config.ProviderConfig, opts provider.Options, log *slog.Logger) (*provider.Registry, error) {
	reg := provider.NewRegistry()
	for _, c := range cfgs {
		var p provider.Provider
		switch c.Type {
		case config.TypePassthrough:
			p = passthrough.New(c, opts)
		case config.TypeAnthropic:
			p = anthropic.New(c, opts)
		case config.TypeGemini:
			p = gemini.New(c, opts)
		case config.TypeCohere:
			p = cohere.New(c, opts)
		case config.TypeBedrock:
			p = bedrock.New(c, opts)
		case config.TypeAzure:
			p = azure.New(c, opts)
		default:
			if log != nil {
				log.Warn("skipping provider: no adapter for type yet", "provider", c.Name, "type", string(c.Type))
			}
			continue
		}
		if err := reg.Register(p); err != nil {
			return nil, err
		}
		if s := Stability(c.Type); s != "stable" && log != nil {
			log.Warn("provider is not yet verified against the live API",
				"provider", c.Name, "type", string(c.Type), "stability", s)
		}
	}
	return reg, nil
}

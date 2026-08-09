package gateway

import (
	"context"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/provider"
)

// This file implements the BYOK-vs-central key resolution model.
//
// Every authenticated request resolves, per the provider it routes to, to one of:
//
//   - BYOK (bring your own key): the ACCOUNT'S OWN provider key. The gateway
//     calls the provider directly with that key and the request is NOT metered
//     or billed to the Vulos control plane.
//   - CENTRAL (default): the provider's statically-configured Vulos key. The
//     request IS metered and (with the cp seam wired) billed.
//
// Resolution is per (account, provider) because BYOK is set per provider. The
// store is consulted at dispatch time, where the routed provider is known.

// BYOKStore is the subset of byok.Store the gateway needs. Defining it here
// keeps the core free of a hard dependency on a concrete store (tests inject a
// fake; cmd/llmux wires core/byok). A nil store means BYOK is disabled and every
// request uses central keys.
type BYOKStore interface {
	// Get returns the account's API key for provider, or ("", false).
	Get(account, provider string) (string, bool)
	// Set stores (encrypts) the account's API key for provider.
	Set(account, provider, apiKey string) error
	// Clear removes the account's BYOK key for provider.
	Clear(account, provider string) error
	// Providers lists the provider names the account has BYOK keys for.
	Providers(account string) []string
}

// SetBYOKStore wires a BYOK credential store. nil is ignored (BYOK stays off).
func (g *Gateway) SetBYOKStore(store BYOKStore) {
	if store != nil {
		g.byok = store
	}
}

// BYOK returns the wired BYOK store, or nil when BYOK is disabled.
func (g *Gateway) BYOK() BYOKStore { return g.byok }

// ByokEligible reports whether a provider CAN serve BYOK requests. A provider is
// eligible only when its adapter honors the per-request key override
// (provider.ResolveKey). Bedrock authenticates with AWS SigV4 credentials rather
// than a single bearer key, so it is central-only — treating a Bedrock request as
// BYOK would silently use the central AWS credentials yet skip metering. Unknown
// providers are treated as eligible (passthrough-shaped).
func (g *Gateway) ByokEligible(providerName string) bool {
	if pc, ok := g.cfg.ProviderByName(providerName); ok {
		return pc.Type != config.TypeBedrock
	}
	return true
}

// resolveCredential decides BYOK vs central for (the request's account,
// providerName). When the account has an eligible BYOK key for that provider it
// returns a context carrying the key (so the adapter uses it) and byok=true.
// Otherwise it returns the context unchanged and byok=false (central path).
func (g *Gateway) resolveCredential(ctx context.Context, providerName string) (context.Context, bool) {
	if g.byok == nil {
		return ctx, false
	}
	account := accountFrom(ctx)
	if account == "" {
		return ctx, false
	}
	if !g.ByokEligible(providerName) {
		return ctx, false
	}
	key, ok := g.byok.Get(account, providerName)
	if !ok || key == "" {
		return ctx, false
	}
	return provider.WithAPIKey(ctx, key), true
}

// ResolveCredential is the exported form of resolveCredential, for dispatch
// paths outside this package (the HTTP shell's modality/transcription forwards).
func (g *Gateway) ResolveCredential(ctx context.Context, providerName string) (context.Context, bool) {
	return g.resolveCredential(ctx, providerName)
}

// primaryBYOK reports whether the account would use BYOK for a route's primary
// provider. Used for the metering decision on cache hits (no provider is called),
// where the served provider is otherwise unknown.
func (g *Gateway) primaryBYOK(ctx context.Context, providerName string) bool {
	_, byok := g.resolveCredential(ctx, providerName)
	return byok
}

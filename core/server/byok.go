package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/openai"
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
func (s *Server) SetBYOKStore(store BYOKStore) {
	if store != nil {
		s.byok = store
	}
}

// byokEligible reports whether a provider CAN serve BYOK requests. A provider is
// eligible only when its adapter honors the per-request key override
// (provider.ResolveKey). Bedrock authenticates with AWS SigV4 credentials rather
// than a single bearer key, so it is central-only — treating a Bedrock request as
// BYOK would silently use the central AWS credentials yet skip metering. Unknown
// providers are treated as eligible (passthrough-shaped).
func (s *Server) byokEligible(providerName string) bool {
	if pc, ok := s.cfg.ProviderByName(providerName); ok {
		return pc.Type != config.TypeBedrock
	}
	return true
}

// resolveCredential decides BYOK vs central for (the request's account,
// providerName). When the account has an eligible BYOK key for that provider it
// returns a context carrying the key (so the adapter uses it) and byok=true.
// Otherwise it returns the context unchanged and byok=false (central path).
func (s *Server) resolveCredential(ctx context.Context, providerName string) (context.Context, bool) {
	if s.byok == nil {
		return ctx, false
	}
	account := accountFrom(ctx)
	if account == "" {
		return ctx, false
	}
	if !s.byokEligible(providerName) {
		return ctx, false
	}
	key, ok := s.byok.Get(account, providerName)
	if !ok || key == "" {
		return ctx, false
	}
	return provider.WithAPIKey(ctx, key), true
}

// primaryBYOK reports whether the account would use BYOK for a route's primary
// provider. Used for the metering decision on cache hits (no provider is called),
// where the served provider is otherwise unknown.
func (s *Server) primaryBYOK(ctx context.Context, providerName string) bool {
	_, byok := s.resolveCredential(ctx, providerName)
	return byok
}

// ---------------------------------------------------------------------------
// Admin endpoints: /admin/byok/... (master-key gated by authMW's /admin guard).
// ---------------------------------------------------------------------------

// registerBYOKRoutes mounts the per-account BYOK management endpoints. They are
// always mounted; when no store is configured they report 501 so the contract is
// discoverable. Secrets are write-only: a stored key is never returned.
func (s *Server) registerBYOKRoutes() {
	s.mux.HandleFunc("GET /admin/byok/{account}", s.handleBYOKList)
	s.mux.HandleFunc("PUT /admin/byok/{account}/{provider}", s.handleBYOKSet)
	s.mux.HandleFunc("DELETE /admin/byok/{account}/{provider}", s.handleBYOKClear)
}

// byokDisabled writes the standard 501 when no store is wired.
func (s *Server) byokDisabled(w http.ResponseWriter) bool {
	if s.byok == nil {
		writeError(w, http.StatusNotImplemented,
			openai.NewError("BYOK is not enabled on this gateway (set LLMUX_BYOK_KEK)", "invalid_request_error", "byok_disabled"))
		return true
	}
	return false
}

type byokSetRequest struct {
	APIKey string `json:"api_key"`
}

// handleBYOKList returns the provider names an account has BYOK keys for (never
// the keys). The response also marks central-only (ineligible) configured
// providers so callers know where BYOK does not apply.
func (s *Server) handleBYOKList(w http.ResponseWriter, r *http.Request) {
	if s.byokDisabled(w) {
		return
	}
	account := r.PathValue("account")
	writeJSON(w, http.StatusOK, map[string]any{
		"account":   account,
		"providers": s.byok.Providers(account),
	})
}

// handleBYOKSet stores (encrypts) an account's BYOK key for a provider.
func (s *Server) handleBYOKSet(w http.ResponseWriter, r *http.Request) {
	if s.byokDisabled(w) {
		return
	}
	account := r.PathValue("account")
	prov := r.PathValue("provider")
	if !s.byokEligible(prov) {
		writeError(w, http.StatusBadRequest,
			openai.NewError("provider "+prov+" does not support BYOK (central-only)", "invalid_request_error", "byok_unsupported_provider"))
		return
	}
	var req byokSetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("invalid JSON body", "invalid_request_error", ""))
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest,
			openai.NewError("api_key is required", "invalid_request_error", "missing_api_key"))
		return
	}
	if err := s.byok.Set(account, prov, req.APIKey); err != nil {
		// Never echo the key or low-level crypto detail; log-free generic message.
		writeError(w, http.StatusBadRequest,
			openai.NewError("could not store BYOK key", "invalid_request_error", "byok_store_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": account, "provider": prov, "mode": "byok",
	})
}

// handleBYOKClear removes an account's BYOK key for a provider (reverts to
// central for that provider).
func (s *Server) handleBYOKClear(w http.ResponseWriter, r *http.Request) {
	if s.byokDisabled(w) {
		return
	}
	account := r.PathValue("account")
	prov := r.PathValue("provider")
	if err := s.byok.Clear(account, prov); err != nil {
		writeError(w, http.StatusBadRequest,
			openai.NewError("could not clear BYOK key", "invalid_request_error", "byok_clear_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": account, "provider": prov, "mode": "central",
	})
}

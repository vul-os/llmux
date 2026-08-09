package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
)

// BYOKStore is the credential store the gateway consults to decide BYOK vs
// central per (account, provider). The interface and the resolution logic live
// in core/gateway; this package keeps the master-key-gated admin endpoints that
// manage it.
type BYOKStore = gateway.BYOKStore

// SetBYOKStore wires a BYOK credential store. nil is ignored (BYOK stays off).
func (s *Server) SetBYOKStore(store BYOKStore) { s.gw.SetBYOKStore(store) }

// byokEligible reports whether a provider CAN serve BYOK requests.
func (s *Server) byokEligible(providerName string) bool { return s.gw.ByokEligible(providerName) }

// resolveCredential decides BYOK vs central for (the request's account, name).
func (s *Server) resolveCredential(ctx context.Context, providerName string) (context.Context, bool) {
	return s.gw.ResolveCredential(ctx, providerName)
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
	if s.gw.BYOK() == nil {
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
		"providers": s.gw.BYOK().Providers(account),
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
	if err := s.gw.BYOK().Set(account, prov, req.APIKey); err != nil {
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
	if err := s.gw.BYOK().Clear(account, prov); err != nil {
		writeError(w, http.StatusBadRequest,
			openai.NewError("could not clear BYOK key", "invalid_request_error", "byok_clear_failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": account, "provider": prov, "mode": "central",
	})
}

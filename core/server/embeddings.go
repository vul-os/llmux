package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/llmux/llmux/core/openai"
)

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("failed to read request body", "invalid_request_error", ""))
		return
	}
	var req openai.EmbeddingRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("invalid JSON: "+err.Error(), "invalid_request_error", ""))
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, openai.NewError("you must provide a model parameter", "invalid_request_error", "missing_model"))
		return
	}
	if k := keyFrom(r.Context()); k != nil && !k.AllowsModel(req.Model) {
		writeError(w, http.StatusForbidden, openai.NewError("model "+req.Model+" not allowed for this key", "invalid_request_error", "model_not_allowed"))
		return
	}
	res, err := s.router.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, openai.NewError(err.Error(), "invalid_request_error", "model_not_found"))
		return
	}
	// Fail closed: never serve a metered request on a budgeted key for a model we
	// cannot price (see unmeterableBudgeted) — uncounted spend would evade budget.
	if s.unmeterableBudgeted(r.Context(), req.Model, res.Primary.Provider.Name()) {
		writeUnmeterable(w, req.Model)
		return
	}
	t := res.Primary
	// Sovereignty gate: deny embeddings to a non-local provider without opt-in.
	if err := s.enforceSovereignty(t.Provider.Name()); err != nil {
		writeProviderError(w, err)
		return
	}
	// BYOK vs central for the routed provider: inject the account's own key when
	// set, and mark the request unmetered.
	callCtx, byok := s.resolveCredential(r.Context(), t.Provider.Name())
	resp, err := t.Provider.Embeddings(callCtx, &req, t.Model, raw)
	if err != nil {
		writeProviderError(w, err)
		return
	}
	if resp.Object == "" {
		resp.Object = "list"
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	// Meter embeddings exactly like chat: compute dollar cost from the pricing
	// catalog (route-aware on the provider used), charge the budget, and log the
	// usage record. Previously embeddings were budget-gated but never metered.
	if resp.Usage == nil {
		resp.Usage = &openai.Usage{}
	}
	s.attachCost(req.Model, t.Provider.Name(), resp.Usage)
	meterCtx := withBYOK(r.Context(), byok)
	s.recordSpend(meterCtx, resp.Usage)
	s.logUsage(meterCtx, req.Model, false, false, resp.Usage)
	writeJSON(w, http.StatusOK, resp)
}

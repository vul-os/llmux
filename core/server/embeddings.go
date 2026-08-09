package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/vul-os/llmux/core/openai"
)

// handleEmbeddings is the HTTP adapter over Gateway.EmbedRaw. Routing, the
// unmeterable-budget refusal, the sovereignty gate, BYOK and metering all
// happen inside the gateway; this maps the outcome onto HTTP.
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
	// Pre-flight failures (missing model, key allow-list, unknown model,
	// unmeterable-on-a-budgeted-key) each map to their own status.
	res, err := s.gw.Prepare(r.Context(), req.Model)
	if err != nil {
		writePrepareError(w, req.Model, err)
		return
	}
	resp, err := s.gw.EmbedRoute(r.Context(), &req, raw, res)
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

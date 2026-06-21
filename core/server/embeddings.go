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
	t := res.Primary
	resp, err := t.Provider.Embeddings(r.Context(), &req, t.Model, raw)
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
	writeJSON(w, http.StatusOK, resp)
}

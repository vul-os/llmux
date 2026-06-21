package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// modalityRoutes maps OpenAI resource routes to the upstream path suffix. These
// carry a "model" field in the JSON body, so they route like chat. Providers
// that implement provider.Forwarder (passthrough) serve them; translating
// adapters return 501.
var modalityRoutes = map[string]string{
	"POST /v1/completions":        "/completions",
	"POST /v1/moderations":        "/moderations",
	"POST /v1/images/generations": "/images/generations",
	"POST /v1/audio/speech":       "/audio/speech",
	"POST /v1/rerank":             "/rerank",
	"POST /v1/responses":          "/responses",
}

// registerModalityRoutes wires the generic forwarders.
func (s *Server) registerModalityRoutes() {
	for pattern, suffix := range modalityRoutes {
		suffix := suffix
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.handleForward(w, r, suffix)
		})
	}
}

// handleForward proxies a model-bearing OpenAI resource request to the routed
// provider via the Forwarder capability.
func (s *Server) handleForward(w http.ResponseWriter, r *http.Request, suffix string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("failed to read request body", "invalid_request_error", ""))
		return
	}
	model := extractModel(raw)
	if model == "" {
		writeError(w, http.StatusBadRequest, openai.NewError("you must provide a model parameter", "invalid_request_error", "missing_model"))
		return
	}
	if k := keyFrom(r.Context()); k != nil && !k.AllowsModel(model) {
		writeError(w, http.StatusForbidden, openai.NewError("model "+model+" not allowed for this key", "invalid_request_error", "model_not_allowed"))
		return
	}
	res, err := s.router.Resolve(model)
	if err != nil {
		writeError(w, http.StatusNotFound, openai.NewError(err.Error(), "invalid_request_error", "model_not_found"))
		return
	}
	t := res.Primary
	fwd, ok := t.Provider.(provider.Forwarder)
	if !ok {
		writeError(w, http.StatusNotImplemented, openai.NewError(
			"provider "+t.Provider.Name()+" does not support "+suffix, "invalid_request_error", "unsupported_endpoint"))
		return
	}
	// Rewrite the model field to the upstream target name.
	body := rewriteModelField(raw, t.Model)

	fr, err := fwd.Forward(r.Context(), provider.ForwardRequest{
		Method: http.MethodPost, Suffix: suffix, Body: body, ContentType: "application/json",
	})
	if err != nil {
		s.metrics.incUpstreamErr()
		writeProviderError(w, err)
		return
	}
	defer fr.Body.Close()
	copyForward(w, fr)
}

// extractModel reads the "model" field from a JSON body, best-effort.
func extractModel(raw []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Model
}

// rewriteModelField sets the "model" field in a JSON body to target (single pass).
func rewriteModelField(raw []byte, target string) []byte {
	return provider.SetJSONFields(raw, map[string]any{"model": target})
}

// copyForward relays the upstream response, streaming the body (flushing for SSE).
func copyForward(w http.ResponseWriter, fr *provider.ForwardResponse) {
	for k, vs := range fr.Header {
		if hopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(fr.Status)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := fr.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// hopByHop reports whether a header should not be forwarded.
func hopByHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Trailer", "Upgrade", "Content-Length":
		return true
	}
	return false
}

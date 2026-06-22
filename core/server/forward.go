package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

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
	status := fr.Status
	// Relay the response while tapping any usage the upstream reported, then meter
	// it. Modality routes (/v1/completions, /v1/responses, images, moderations,
	// audio, rerank) were gated but billed nobody; now every SERVED forward emits
	// a usage record so it is auditable even when the catalog has no price (the
	// record then carries tokens=0, cost=0, with the model). Upstream errors carry
	// no spend, so they are relayed but not metered.
	usage, served := copyForwardMetered(w, fr)
	if status < 200 || status >= 300 {
		return
	}
	stream := isStreamRequest(raw)
	if usage == nil {
		// No usage parsed from the (bounded) tap. For a streamed forward this can
		// mean the upstream's usage chunk landed beyond maxMeterTapBytes on a large
		// stream — billing it as cost=0 would silently drop revenue. Estimate from
		// the prompt text + total bytes actually served so large forwards are still
		// metered. Non-streamed bodies with no usage (e.g. images/audio) stay a
		// zero-token auditable line.
		if stream && served > 0 {
			usage = estimateForwardUsage(raw, served)
		} else {
			usage = &openai.Usage{}
		}
	}
	s.attachCost(model, t.Provider.Name(), usage)
	s.recordSpend(r.Context(), usage)
	s.logUsage(r.Context(), model, stream, false, usage)
}

// isStreamRequest best-effort reports whether the request body asked to stream.
func isStreamRequest(raw []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Stream
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

// maxMeterTapBytes bounds how much of a forwarded response we retain to parse
// usage. Usage objects are tiny; capping keeps metering memory-safe for large
// (e.g. image/base64) bodies. Beyond the cap we stop tapping but keep relaying.
const maxMeterTapBytes = 1 << 20 // 1 MiB

// copyForwardMetered relays the upstream response byte-for-byte (flushing for
// SSE) while tapping a bounded copy of the body so it can parse any reported
// usage. It returns the parsed usage (nil when the upstream reported none) and
// the TOTAL number of body bytes served (used to estimate metering for large
// streams whose usage chunk fell beyond the bounded tap). Relaying is never
// blocked or altered by the tap.
func copyForwardMetered(w http.ResponseWriter, fr *provider.ForwardResponse) (*openai.Usage, int) {
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

	var tap bytes.Buffer
	served := 0
	buf := make([]byte, 32*1024)
	for {
		n, err := fr.Body.Read(buf)
		if n > 0 {
			served += n
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
			if tap.Len() < maxMeterTapBytes {
				tap.Write(buf[:n])
			}
		}
		if err != nil {
			break
		}
	}
	// Only meter usage on a successful upstream status (errors carry no spend).
	if fr.Status < 200 || fr.Status >= 300 {
		return nil, served
	}
	return extractUsage(tap.Bytes()), served
}

// estimateForwardUsage approximates token usage for a streamed forward whose
// upstream did not surface a parseable usage object within the bounded tap (e.g.
// a large stream whose final usage chunk landed past maxMeterTapBytes). It is a
// deliberate floor — prompt from the request text, completion from the served
// byte count — so large forwards are metered rather than silently billed as
// cost=0. A real usage object, when parsed, always wins.
func estimateForwardUsage(reqRaw []byte, servedBytes int) *openai.Usage {
	prompt := charsToTokens(promptCharsFromBody(reqRaw))
	completion := charsToTokens(servedBytes)
	return &openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// promptCharsFromBody best-effort sums the character length of prompt-like text
// in a forwarded request body (chat messages, a prompt string, or input). Used
// only for the estimate floor, so a loose heuristic is acceptable.
func promptCharsFromBody(raw []byte) int {
	var doc struct {
		Prompt   json.RawMessage `json:"prompt"`
		Input    json.RawMessage `json:"input"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0
	}
	n := len(doc.Prompt) + len(doc.Input)
	for _, m := range doc.Messages {
		n += len(m.Content)
	}
	return n
}

// extractUsage finds an OpenAI-style usage object in a forwarded response body.
// It handles both a plain JSON object (completions/responses/moderations) and an
// SSE stream (it scans data: lines for the chunk that carries usage). Returns
// nil when no usage is present (e.g. images/audio, which the caller then records
// as a zero-token auditable line).
func extractUsage(body []byte) *openai.Usage {
	if u := usageFromJSON(body); u != nil {
		return u
	}
	// SSE: usage rides on a trailing data: line.
	var found *openai.Usage
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		if u := usageFromJSON([]byte(data)); u != nil {
			found = u // keep the last one seen (the final usage chunk)
		}
	}
	return found
}

// usageFromJSON decodes the top-level "usage" object from a JSON document, if
// present and non-empty. /v1/responses nests its counts differently, so it also
// accepts input_tokens/output_tokens.
func usageFromJSON(raw []byte) *openai.Usage {
	var doc struct {
		Usage *struct {
			openai.Usage
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Usage == nil {
		return nil
	}
	u := doc.Usage.Usage
	// /v1/responses: map input/output token names onto the canonical fields.
	if u.PromptTokens == 0 && doc.Usage.InputTokens > 0 {
		u.PromptTokens = doc.Usage.InputTokens
	}
	if u.CompletionTokens == 0 && doc.Usage.OutputTokens > 0 {
		u.CompletionTokens = doc.Usage.OutputTokens
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = doc.Usage.TotalTokens
	}
	if u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	return &u
}

// hopByHop reports whether a header should not be forwarded.
func hopByHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Trailer", "Upgrade", "Content-Length":
		return true
	}
	return false
}

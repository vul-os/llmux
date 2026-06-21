package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/llmux/llmux/core/cache"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
	"github.com/llmux/llmux/core/router"
)

// maxBodyBytes caps request bodies (generous for large multimodal/tool payloads).
const maxBodyBytes = 32 << 20 // 32 MiB

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("failed to read request body", "invalid_request_error", ""))
		return
	}

	var req openai.ChatCompletionRequest
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

	if req.Stream {
		s.streamChat(w, r, &req, raw, res)
		return
	}
	s.unaryChat(w, r, &req, raw, res)
}

func (s *Server) unaryChat(w http.ResponseWriter, r *http.Request, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution) {
	// Exact-match cache lookup (only when enabled).
	var cacheKey string
	if s.cache != nil {
		scope := ""
		if k := keyFrom(r.Context()); k != nil {
			scope = k.Key
		}
		cacheKey = s.cacheKeyFor(req, raw, scope)
		if hit, ok := s.cache.Get(cacheKey); ok {
			w.Header().Set("X-LLMux-Cache", "hit")
			s.metrics.incCacheHit()
			s.logUsage(keyName(r.Context()), req.Model, false, true, hit.Usage)
			writeRawJSON(w, hit.Body) // pre-serialized body; no re-marshal
			return
		}
	}

	// Optional upstream deadline + a sink to relay rate-limit headers.
	ctx := r.Context()
	if s.cfg.UpstreamTimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(s.cfg.UpstreamTimeoutSeconds)*time.Second)
		defer cancel()
	}
	ctx, sink := provider.WithHeaderSink(ctx)

	resp, usedProvider, err := s.dispatchUnary(ctx, req, raw, res)
	if err != nil {
		s.metrics.incUpstreamErr()
		relayHeaders(w, sink) // relay rate-limit/retry-after headers even on error
		writeProviderError(w, err)
		return
	}
	fillResponseDefaults(resp, req.Model)
	s.attachCost(req.Model, usedProvider, resp.Usage)
	s.recordSpend(r.Context(), resp.Usage)
	s.logUsage(keyName(r.Context()), req.Model, false, false, resp.Usage)

	// Serialize once: write it and (if caching) store the bytes — a hit then
	// replays the body verbatim with no re-marshal and no shared response pointer.
	data, err := json.Marshal(resp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, openai.NewError("failed to encode response", "internal_error", ""))
		return
	}
	if s.cache != nil {
		s.cache.Set(cacheKey, &cache.Entry{Body: data, Usage: resp.Usage})
	}
	relayHeaders(w, sink)
	writeRawJSON(w, data)
}

// relayHeaders copies captured upstream headers (rate-limit, retry-after) onto
// the client response.
func relayHeaders(w http.ResponseWriter, sink *provider.HeaderSink) {
	for k, vs := range sink.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution) {
	var sse *sseWriter
	started := false
	var lastUsage *openai.Usage
	var usedProvider string

	makeYield := func(provName string) func(*openai.ChatCompletionChunk) error {
		return func(chunk *openai.ChatCompletionChunk) error {
			if !started {
				var ok bool
				sse, ok = newSSEWriter(w)
				if !ok {
					return io.ErrClosedPipe
				}
				started = true
				usedProvider = provName
			}
			if chunk.Model == "" {
				chunk.Model = req.Model
			}
			if chunk.Object == "" {
				chunk.Object = "chat.completion.chunk"
			}
			if chunk.Usage != nil {
				s.attachCost(req.Model, usedProvider, chunk.Usage)
				lastUsage = chunk.Usage
			}
			return sse.chunk(chunk)
		}
	}

	// Try targets until one begins streaming; failover only before first chunk.
	var lastErr error
	for _, t := range res.All() {
		lastErr = t.Provider.ChatCompletionStream(r.Context(), req, t.Model, raw, makeYield(t.Provider.Name()))
		if lastErr == nil || started {
			break
		}
		if !shouldFailover(lastErr) {
			break
		}
	}

	if lastErr != nil && !started {
		writeProviderError(w, lastErr)
		return
	}
	if lastErr != nil && started {
		body := openai.NewError(lastErr.Error(), "upstream_error", "")
		if pe := asProviderError(lastErr); pe != nil && pe.Body != nil {
			body = pe.Body
		}
		sse.errorEvent(body)
		sse.done()
		return
	}

	if started {
		s.recordSpend(r.Context(), lastUsage)
		s.logUsage(keyName(r.Context()), req.Model, true, false, lastUsage)
		sse.done()
		return
	}
	// No chunks, no error: still emit a valid terminal stream.
	if s2, ok := newSSEWriter(w); ok {
		s2.done()
	}
}

// recordSpend charges the authenticated key for a response's computed cost.
func (s *Server) recordSpend(ctx context.Context, usage *openai.Usage) {
	if usage == nil || usage.Cost == nil {
		return
	}
	if k := keyFrom(ctx); k != nil {
		s.keys.AddSpend(k.Key, usage.Cost.TotalCost)
	}
}

// fillResponseDefaults ensures a non-streaming response has the identity fields
// clients expect even if a provider omitted them.
func fillResponseDefaults(resp *openai.ChatCompletionResponse, model string) {
	if resp.ID == "" {
		resp.ID = genID("chatcmpl-")
	}
	if resp.Object == "" {
		resp.Object = "chat.completion"
	}
	if resp.Created == 0 {
		resp.Created = time.Now().Unix()
	}
	if resp.Model == "" {
		resp.Model = model
	}
}

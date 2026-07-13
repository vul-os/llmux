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
	// Fail closed: never serve a metered request on a budgeted key for a model we
	// cannot price — the spend would go uncounted and the budget would never trip.
	if s.unmeterableBudgeted(r.Context(), req.Model, res.Primary.Provider.Name()) {
		writeUnmeterable(w, req.Model)
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
		cacheKey = s.cacheKeyFor(req, raw, cacheScope(r.Context()))
		if hit, ok := s.cache.Get(cacheKey); ok {
			w.Header().Set("X-LLMux-Cache", "hit")
			s.metrics.incCacheHit()
			// No provider is called on a cache hit; attribute the metering decision
			// to the route's primary provider's BYOK status so a BYOK account's
			// cache hit is recorded unmetered (and never billed to the control plane).
			hitCtx := withBYOK(r.Context(), s.primaryBYOK(r.Context(), res.Primary.Provider.Name()))
			s.logUsage(hitCtx, req.Model, false, true, hit.Usage)
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

	resp, usedProvider, byok, err := s.dispatchUnary(ctx, req, raw, res)
	if err != nil {
		s.metrics.incUpstreamErr()
		relayHeaders(w, sink) // relay rate-limit/retry-after headers even on error
		writeProviderError(w, err)
		return
	}
	fillResponseDefaults(resp, req.Model)
	s.attachCost(req.Model, usedProvider, resp.Usage)
	// Meter against the provider that actually served: BYOK is unmetered.
	meterCtx := withBYOK(r.Context(), byok)
	s.recordSpend(meterCtx, resp.Usage)
	s.logUsage(meterCtx, req.Model, false, false, resp.Usage)

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
	// Force upstream to emit a final usage chunk so streaming chat is ALWAYS
	// metered. Previously usage was only recorded when the client opted in via
	// stream_options.include_usage; otherwise count=0 billed nobody. We inject
	// include_usage server-side (in both the typed request and the raw body the
	// passthrough providers forward verbatim) and, as a belt-and-braces fallback
	// for upstreams that still omit usage, estimate tokens from the streamed text.
	req.Stream = true
	if req.StreamOptions == nil {
		req.StreamOptions = &openai.StreamOptions{}
	}
	req.StreamOptions.IncludeUsage = true
	raw = provider.SetJSONFields(raw, map[string]any{"stream_options": map[string]any{"include_usage": true}})

	var sse *sseWriter
	started := false
	var lastUsage *openai.Usage
	var usedProvider string
	var usedBYOK bool    // whether the serving provider used the account's own key
	var contentChars int // accumulated streamed text, for the fallback estimate

	makeYield := func(provName string, byok bool) func(*openai.ChatCompletionChunk) error {
		return func(chunk *openai.ChatCompletionChunk) error {
			if !started {
				var ok bool
				sse, ok = newSSEWriter(w)
				if !ok {
					return io.ErrClosedPipe
				}
				started = true
				usedProvider = provName
				usedBYOK = byok
			}
			if chunk.Model == "" {
				chunk.Model = req.Model
			}
			if chunk.Object == "" {
				chunk.Object = "chat.completion.chunk"
			}
			for _, ch := range chunk.Choices {
				contentChars += len(ch.Delta.Content)
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
		// Sovereignty gate (see dispatchUnary): skip a blocked non-local target
		// before any connection; a local fallback may still stream.
		if err := s.enforceSovereignty(t.Provider.Name()); err != nil {
			lastErr = err
			continue
		}
		// Resolve BYOK vs central per target so the right key is used and the
		// metering decision follows the provider that actually serves.
		callCtx, byok := s.resolveCredential(r.Context(), t.Provider.Name())
		lastErr = t.Provider.ChatCompletionStream(callCtx, req, t.Model, raw, makeYield(t.Provider.Name(), byok))
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
		// Tokens were already served to the client before the stream failed
		// (client disconnect / pipe failure / mid-stream upstream error). Meter
		// what was served so far rather than billing nobody: prefer the upstream's
		// last reported usage, else fall back to the streamed-text estimate.
		metered := lastUsage
		if metered == nil {
			metered = estimateStreamUsage(req, contentChars)
			s.attachCost(req.Model, usedProvider, metered)
		}
		meterCtx := withBYOK(r.Context(), usedBYOK)
		s.recordSpend(meterCtx, metered)
		s.logUsage(meterCtx, req.Model, true, false, metered)

		body := openai.NewError(lastErr.Error(), "upstream_error", "")
		if pe := asProviderError(lastErr); pe != nil && pe.Body != nil {
			body = pe.Body
		}
		sse.errorEvent(body)
		sse.done()
		return
	}

	if started {
		// Fallback: upstream sent no usage chunk despite include_usage. Estimate
		// tokens (prompt from the request text, completion from streamed content)
		// so the stream is still metered rather than billed as free.
		if lastUsage == nil {
			lastUsage = estimateStreamUsage(req, contentChars)
			s.attachCost(req.Model, usedProvider, lastUsage)
		}
		meterCtx := withBYOK(r.Context(), usedBYOK)
		s.recordSpend(meterCtx, lastUsage)
		s.logUsage(meterCtx, req.Model, true, false, lastUsage)
		sse.done()
		return
	}
	// No chunks, no error: still emit a valid terminal stream.
	if s2, ok := newSSEWriter(w); ok {
		s2.done()
	}
}

// estimateStreamUsage approximates token usage for a streamed completion when
// the upstream omitted a usage chunk despite include_usage. It uses the common
// ~4-chars-per-token heuristic: prompt from the request message text, completion
// from the accumulated streamed content. This is a deliberate floor so streaming
// is never billed as zero; an upstream that reports real usage always wins.
func estimateStreamUsage(req *openai.ChatCompletionRequest, completionChars int) *openai.Usage {
	promptChars := 0
	for _, m := range req.Messages {
		promptChars += len(m.Content.String())
	}
	prompt := charsToTokens(promptChars)
	completion := charsToTokens(completionChars)
	return &openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// charsToTokens converts a character count to an approximate token count
// (~4 chars/token), rounding up so any non-empty text costs at least 1 token.
func charsToTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

// recordSpend charges the authenticated key for a response's computed cost.
// BYOK requests are unmetered: they consume the account's own provider key, so
// no central spend is recorded against the static key budget.
func (s *Server) recordSpend(ctx context.Context, usage *openai.Usage) {
	if usage == nil || usage.Cost == nil || byokFrom(ctx) {
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

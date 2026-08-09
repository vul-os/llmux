package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/vul-os/llmux/core/cache"
	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/provider"
	"github.com/vul-os/llmux/core/router"
)

// Result is one completed non-streaming chat request. Everything in it was
// already computed on the way through — the HTTP shell used to throw it
// straight at a ResponseWriter, which is why an in-process caller could not see
// which provider actually served, whether it was BYOK, or the upstream's
// rate-limit headers.
type Result struct {
	// Response is the canonical OpenAI response, with identity fields filled and
	// usage.cost attached from the price catalog.
	Response *openai.ChatCompletionResponse
	// Provider is the provider that ACTUALLY served, after any failover.
	Provider string
	// BYOK is true when the request was served on the account's own provider key
	// (and is therefore unmetered).
	BYOK bool
	// CacheHit is true when the response came from the exact/semantic cache and
	// no provider was called.
	CacheHit bool
	// Headers carries the allow-listed upstream headers (rate-limit, retry-after)
	// the provider.WithHeaderSink relay captured. Nil on a cache hit.
	Headers http.Header
	// Body is the response serialized exactly once. The HTTP shell writes these
	// bytes verbatim rather than re-marshaling, and a cache hit replays them.
	Body []byte
}

// ErrNoModel is returned when a request omits the required model parameter.
var ErrNoModel = errors.New("llmux: you must provide a model parameter")

// ErrModelNotAllowed is returned when the authenticated key's allow-list does
// not include the requested model.
var ErrModelNotAllowed = errors.New("llmux: model not allowed for this key")

// UnmeterableError is returned pre-flight when serving a request would be an
// UNMETERABLE charge against a budget-enforcing key (see unmeterableBudgeted).
type UnmeterableError struct{ Model string }

func (e *UnmeterableError) Error() string {
	return "llmux: model " + e.Model +
		" has no configured price; refusing to serve an unmeterable request against a budgeted key"
}

// Chat performs a non-streaming chat completion in-process: cache lookup,
// routing, sovereignty gate, BYOK resolution, retries, failover, cost
// attribution, spend recording and usage logging — the whole path the HTTP
// endpoint runs, minus HTTP.
//
// It marshals req itself. The HTTP shell calls ChatRaw instead so the client's
// ORIGINAL bytes reach the provider unchanged, which is how passthrough
// preserves fields llmux does not model.
func (g *Gateway) Chat(ctx context.Context, req *openai.ChatCompletionRequest) (*Result, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return g.ChatRaw(ctx, req, raw)
}

// ChatRaw is Chat with the caller's original request bytes. Providers forward
// raw near-verbatim (swapping only model/stream), so unknown upstream fields
// survive the hop. Pass nil raw to have it marshaled from req.
func (g *Gateway) ChatRaw(ctx context.Context, req *openai.ChatCompletionRequest, raw []byte) (*Result, error) {
	res, err := g.prepare(ctx, req.Model, len(raw) == 0)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		if raw, err = json.Marshal(req); err != nil {
			return nil, err
		}
	}
	return g.unaryChat(ctx, req, raw, res)
}

// prepare runs the checks every chat/embeddings request shares: key allow-list,
// routing, and the unmeterable-budget refusal. It returns the resolved route.
func (g *Gateway) prepare(ctx context.Context, model string, _ bool) (router.Resolution, error) {
	if model == "" {
		return router.Resolution{}, ErrNoModel
	}
	if k := keyFrom(ctx); k != nil && !k.AllowsModel(model) {
		return router.Resolution{}, ErrModelNotAllowed
	}
	res, err := g.router.Resolve(model)
	if err != nil {
		return router.Resolution{}, err
	}
	// Fail closed: never serve a metered request on a budgeted key for a model we
	// cannot price — the spend would go uncounted and the budget would never trip.
	if g.unmeterableBudgeted(ctx, model, res.Primary.Provider.Name()) {
		return router.Resolution{}, &UnmeterableError{Model: model}
	}
	return res, nil
}

// Prepare is the exported form of prepare, for the HTTP shell (which maps each
// failure to its own status code) and for callers that want to resolve a route
// without dispatching.
func (g *Gateway) Prepare(ctx context.Context, model string) (router.Resolution, error) {
	return g.prepare(ctx, model, false)
}

// unaryChat is the non-streaming dispatch body: cache lookup, upstream deadline,
// header sink, dispatch with retry/failover, cost attribution, metering, and a
// single serialization reused by the cache and the caller.
func (g *Gateway) unaryChat(ctx context.Context, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution) (*Result, error) {
	// Exact-match cache lookup (only when enabled).
	var cacheKey string
	if g.cache != nil {
		cacheKey = g.cacheKeyFor(req, raw, cacheScope(ctx))
		if hit, ok := g.cache.Get(cacheKey); ok {
			g.metrics.incCacheHit()
			// No provider is called on a cache hit; attribute the metering decision
			// to the route's primary provider's BYOK status so a BYOK account's
			// cache hit is recorded unmetered (and never billed to the control plane).
			byok := g.primaryBYOK(ctx, res.Primary.Provider.Name())
			hitCtx := withBYOK(ctx, byok)
			g.logUsage(hitCtx, req.Model, false, true, hit.Usage)
			out := &Result{
				Provider: res.Primary.Provider.Name(), BYOK: byok, CacheHit: true, Body: hit.Body,
			}
			// Decode the cached bytes for callers that want the typed response.
			// The bytes stay authoritative (they are what the HTTP shell writes).
			var resp openai.ChatCompletionResponse
			if err := json.Unmarshal(hit.Body, &resp); err == nil {
				out.Response = &resp
			}
			return out, nil
		}
	}

	// Optional upstream deadline + a sink to relay rate-limit headers.
	if g.cfg.UpstreamTimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(g.cfg.UpstreamTimeoutSeconds)*time.Second)
		defer cancel()
	}
	callCtx, sink := provider.WithHeaderSink(ctx)

	resp, usedProvider, byok, err := g.dispatchUnary(callCtx, req, raw, res)
	if err != nil {
		g.metrics.incUpstreamErr()
		// Relay rate-limit/retry-after headers even on error.
		return &Result{Headers: sink.Header()}, err
	}
	fillResponseDefaults(resp, req.Model)
	g.attachCost(req.Model, usedProvider, resp.Usage)
	// Meter against the provider that actually served: BYOK is unmetered.
	meterCtx := withBYOK(ctx, byok)
	g.recordSpend(meterCtx, resp.Usage)
	g.logUsage(meterCtx, req.Model, false, false, resp.Usage)

	// Serialize once: the caller writes it and (if caching) we store the bytes —
	// a hit then replays the body verbatim with no re-marshal and no shared
	// response pointer.
	data, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	if g.cache != nil {
		g.cache.Set(cacheKey, &cache.Entry{Body: data, Usage: resp.Usage})
	}
	return &Result{
		Response: resp, Provider: usedProvider, BYOK: byok,
		Headers: sink.Header(), Body: data,
	}, nil
}

// StreamSink receives the streamed chunks. It mirrors what the HTTP shell needs
// (an SSE writer) and what an in-process caller needs (a callback), without the
// gateway knowing which it is talking to.
type StreamSink interface {
	// Open commits the response (for SSE: writes the headers) before the first
	// chunk. Returning an error aborts the stream BEFORE anything was served, so
	// the caller can still answer with an ordinary error response.
	Open() error
	// Chunk delivers one chunk. Returning an error aborts the stream.
	Chunk(*openai.ChatCompletionChunk) error
	// Failed is called when the stream broke AFTER chunks were already served.
	// The tokens served so far have been metered by then.
	Failed(*openai.ErrorResponse)
	// Done is called once the stream ends cleanly (or after Failed).
	Done()
}

// ChatStream performs a streaming chat completion, invoking yield for each
// chunk. It is the callback form of the streaming path for in-process callers;
// the HTTP shell uses ChatStreamSink so it can emit SSE error events and the
// terminal [DONE] frame.
func (g *Gateway) ChatStream(ctx context.Context, req *openai.ChatCompletionRequest, yield provider.ChunkFunc) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	res, err := g.prepare(ctx, req.Model, false)
	if err != nil {
		return err
	}
	return g.ChatStreamSink(ctx, req, raw, res, funcSink{yield: yield})
}

// funcSink adapts a plain ChunkFunc to StreamSink for in-process callers, which
// see a mid-stream failure as the returned error rather than an SSE event.
type funcSink struct{ yield provider.ChunkFunc }

func (f funcSink) Open() error                               { return nil }
func (f funcSink) Chunk(c *openai.ChatCompletionChunk) error { return f.yield(c) }
func (f funcSink) Failed(*openai.ErrorResponse)              {}
func (f funcSink) Done()                                     {}

// ChatStreamSink is the streaming dispatch body. It forces a final usage chunk
// so streaming is ALWAYS metered, fails over between targets until one begins
// streaming, and meters whatever was served even when the stream breaks
// mid-flight.
//
// It returns the error that ended the stream, if any. When chunks were already
// served the sink's Failed is called (the HTTP shell turns that into an SSE
// error event) and the error is still returned so the caller knows.
func (g *Gateway) ChatStreamSink(ctx context.Context, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution, sink StreamSink) error {
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
	raw = g.popts.SetJSONFields(raw, map[string]any{"stream_options": map[string]any{"include_usage": true}})

	started := false
	var lastUsage *openai.Usage
	var usedProvider string
	var usedBYOK bool    // whether the serving provider used the account's own key
	var contentChars int // accumulated streamed text, for the fallback estimate

	makeYield := func(provName string, byok bool) func(*openai.ChatCompletionChunk) error {
		return func(chunk *openai.ChatCompletionChunk) error {
			if !started {
				// Commit the response before anything is served. A sink that
				// cannot open (e.g. an unflushable ResponseWriter) aborts here
				// with started still false, so the caller answers with a normal
				// error rather than a half-written stream.
				if err := sink.Open(); err != nil {
					return err
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
				g.attachCost(req.Model, usedProvider, chunk.Usage)
				lastUsage = chunk.Usage
			}
			return sink.Chunk(chunk)
		}
	}

	// Try targets until one begins streaming; failover only before first chunk.
	var lastErr error
	for _, t := range res.All() {
		// Sovereignty gate (see dispatchUnary): skip a blocked non-local target
		// before any connection; a local fallback may still stream.
		if err := g.enforceSovereignty(t.Provider.Name()); err != nil {
			lastErr = err
			continue
		}
		// Resolve BYOK vs central per target so the right key is used and the
		// metering decision follows the provider that actually serves.
		callCtx, byok := g.resolveCredential(ctx, t.Provider.Name())
		lastErr = t.Provider.ChatCompletionStream(callCtx, req, t.Model, raw, makeYield(t.Provider.Name(), byok))
		if lastErr == nil || started {
			break
		}
		if !shouldFailover(lastErr) {
			break
		}
	}

	if lastErr != nil && !started {
		return lastErr
	}
	if lastErr != nil && started {
		// Tokens were already served to the client before the stream failed
		// (client disconnect / pipe failure / mid-stream upstream error). Meter
		// what was served so far rather than billing nobody: prefer the upstream's
		// last reported usage, else fall back to the streamed-text estimate.
		metered := lastUsage
		if metered == nil {
			metered = estimateStreamUsage(req, contentChars)
			g.attachCost(req.Model, usedProvider, metered)
		}
		meterCtx := withBYOK(ctx, usedBYOK)
		g.recordSpend(meterCtx, metered)
		g.logUsage(meterCtx, req.Model, true, false, metered)

		body := openai.NewError(lastErr.Error(), "upstream_error", "")
		if pe := AsProviderError(lastErr); pe != nil && pe.Body != nil {
			body = pe.Body
		}
		sink.Failed(body)
		sink.Done()
		return lastErr
	}

	if started {
		// Fallback: upstream sent no usage chunk despite include_usage. Estimate
		// tokens (prompt from the request text, completion from streamed content)
		// so the stream is still metered rather than billed as free.
		if lastUsage == nil {
			lastUsage = estimateStreamUsage(req, contentChars)
			g.attachCost(req.Model, usedProvider, lastUsage)
		}
		meterCtx := withBYOK(ctx, usedBYOK)
		g.recordSpend(meterCtx, lastUsage)
		g.logUsage(meterCtx, req.Model, true, false, lastUsage)
	}
	// No chunks and no error still ends in a valid terminal stream.
	sink.Done()
	return nil
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
	prompt := CharsToTokens(promptChars)
	completion := CharsToTokens(completionChars)
	return &openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// CharsToTokens converts a character count to an approximate token count
// (~4 chars/token), rounding up so any non-empty text costs at least 1 token.
func CharsToTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

// recordSpend charges the authenticated key for a response's computed cost.
// BYOK requests are unmetered: they consume the account's own provider key, so
// no central spend is recorded against the static key budget.
func (g *Gateway) recordSpend(ctx context.Context, usage *openai.Usage) {
	if usage == nil || usage.Cost == nil || byokFrom(ctx) {
		return
	}
	if k := keyFrom(ctx); k != nil {
		g.keys.AddSpend(k.Key, usage.Cost.TotalCost)
	}
}

// RecordSpend charges the authenticated key for a computed cost. It is the
// exported form of recordSpend, for dispatch paths outside this package.
func (g *Gateway) RecordSpend(ctx context.Context, usage *openai.Usage) { g.recordSpend(ctx, usage) }

// fillResponseDefaults ensures a non-streaming response has the identity fields
// clients expect even if a provider omitted them.
func fillResponseDefaults(resp *openai.ChatCompletionResponse, model string) {
	if resp.ID == "" {
		resp.ID = GenID("chatcmpl-")
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

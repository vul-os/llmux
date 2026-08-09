package gateway

import (
	"context"
	"encoding/json"

	"github.com/vul-os/llmux/core/openai"
)

// Embed performs an embeddings request in-process: routing, the unmeterable
// budget refusal, the sovereignty gate, BYOK resolution, cost attribution,
// spend recording and usage logging.
//
// It marshals req itself; EmbedRaw takes the caller's original bytes so unknown
// upstream fields survive the hop (the HTTP shell uses that).
func (g *Gateway) Embed(ctx context.Context, req *openai.EmbeddingRequest) (*openai.EmbeddingResponse, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return g.EmbedRaw(ctx, req, raw)
}

// EmbedRaw is Embed with the caller's original request bytes.
func (g *Gateway) EmbedRaw(ctx context.Context, req *openai.EmbeddingRequest, raw []byte) (*openai.EmbeddingResponse, error) {
	res, err := g.prepare(ctx, req.Model, false)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		if raw, err = json.Marshal(req); err != nil {
			return nil, err
		}
	}
	t := res.Primary
	// Sovereignty gate: deny embeddings to a non-local provider without opt-in.
	if err := g.enforceSovereignty(t.Provider.Name()); err != nil {
		return nil, err
	}
	// BYOK vs central for the routed provider: inject the account's own key when
	// set, and mark the request unmetered.
	callCtx, byok := g.resolveCredential(ctx, t.Provider.Name())
	resp, err := t.Provider.Embeddings(callCtx, req, t.Model, raw)
	if err != nil {
		return nil, err
	}
	if resp.Object == "" {
		resp.Object = "list"
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	// Meter embeddings exactly like chat: compute dollar cost from the pricing
	// catalog (route-aware on the provider used), charge the budget, and log the
	// usage record.
	if resp.Usage == nil {
		resp.Usage = &openai.Usage{}
	}
	g.attachCost(req.Model, t.Provider.Name(), resp.Usage)
	meterCtx := withBYOK(ctx, byok)
	g.recordSpend(meterCtx, resp.Usage)
	g.logUsage(meterCtx, req.Model, false, false, resp.Usage)
	return resp, nil
}

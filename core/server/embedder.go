package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/llmux/llmux/core/cache"
	"github.com/llmux/llmux/core/keys"
	"github.com/llmux/llmux/core/openai"
)

// serverEmbedder implements cache.Embedder by calling the gateway's own
// embeddings route in-process (no HTTP hop, no auth). Used by the semantic cache.
type serverEmbedder struct {
	s     *Server
	model string
}

// Embed embeds text via the configured embedding model.
func (e serverEmbedder) Embed(ctx context.Context, text string) ([]float64, error) {
	res, err := e.s.router.Resolve(e.model)
	if err != nil {
		return nil, err
	}
	input, _ := json.Marshal(text)
	req := &openai.EmbeddingRequest{Model: e.model, Input: input}
	raw, _ := json.Marshal(req)
	t := res.Primary
	resp, err := t.Provider.Embeddings(ctx, req, t.Model, raw)
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Data) == 0 {
		return nil, fmt.Errorf("embeddings: empty response")
	}
	return resp.Data[0].Embedding, nil
}

// cacheScope returns the per-tenant cache scope for a request so cached
// responses are NEVER shared across accounts (cross-tenant isolation), whether
// the request was authenticated by a static key or resolved by the control
// plane.
//
//   - Static key: sha256(key.Key) — unique per virtual key, never the raw token.
//     Using the hash ensures a Redis SCAN/MONITOR of the cache keyspace never
//     exposes live bearer credentials (at-rest secret protection).
//   - CP-resolved principal (no static Key): the resolved account id. Without
//     this, every cp principal would scope to "" and could be served another
//     account's cached — and, with semantic caching, merely SIMILAR — content.
//   - Genuinely unauthenticated (open/local mode): "" (a single shared scope).
func cacheScope(ctx context.Context) string {
	if k := keyFrom(ctx); k != nil {
		return keys.HashToken(k.Key)
	}
	return accountFrom(ctx)
}

// cacheKeyFor returns the cache lookup key for a request, scoped by the caller's
// tenant (see cacheScope) so neither virtual keys nor cp-resolved accounts ever
// share cached responses (cross-tenant isolation). For semantic caching it
// returns canonical prompt text (which gets embedded); for exact caching, a body
// hash. The scope is prefixed in both modes so the isolation holds for the
// semantic cache too.
func (s *Server) cacheKeyFor(req *openai.ChatCompletionRequest, raw []byte, scope string) string {
	if s.semantic {
		return scope + "\x00" + canonicalText(req)
	}
	return scope + ":" + cache.KeyFor(raw)
}

// canonicalText flattens a request into a stable text representation for
// embedding (scoped by model so different models don't share entries).
func canonicalText(req *openai.ChatCompletionRequest) string {
	var b strings.Builder
	b.WriteString(req.Model)
	for i := range req.Messages {
		m := &req.Messages[i]
		b.WriteString("\n")
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content.String())
	}
	return b.String()
}

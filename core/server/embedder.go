package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/llmux/llmux/core/cache"
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

// cacheKeyFor returns the cache lookup key for a request, scoped by the calling
// key so virtual keys never share cached responses (cross-tenant isolation).
// scope is the authenticated key (empty when unauthenticated). For semantic
// caching it returns canonical prompt text (which gets embedded); for exact
// caching, a body hash.
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

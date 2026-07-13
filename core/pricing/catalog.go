// Package pricing maintains the model price catalog and computes request cost.
//
// llmux's catalog is auto-synced from live sources (OpenRouter, LiteLLM's open
// JSON, provider pricing APIs) rather than hand-maintained. Each source is kept
// separately and merged by precedence (see source.go), so cost is correct per
// route: manual overrides always win, calls routed through a provider use that
// provider's own price, and direct routes prefer authoritative direct prices
// over margin-inclusive aggregator prices. A built-in seed ships so cost works
// offline before any sync.
package pricing

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llmux/llmux/core/openai"
)

// Price is the per-model pricing and capability record. Costs are USD per
// 1,000,000 tokens (per-MTok), the unit humans reason about.
type Price struct {
	Model         string  `json:"model"`
	Provider      string  `json:"provider,omitempty"`
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// CacheReadPerMTok is the discounted rate for cached (prompt-cache hit)
	// input tokens (0 = bill cached tokens at the normal input rate).
	CacheReadPerMTok float64  `json:"cache_read_per_mtok,omitempty"`
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxOutput        int      `json:"max_output,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
}

// sourceData is one source's contribution to the catalog.
type sourceData struct {
	Priority int              `json:"priority"`
	Prices   map[string]Price `json:"prices"`
}

// snapshot is an immutable view of the catalog. Read paths load it once via the
// atomic pointer and read its maps without locking; SetSource builds a brand-new
// snapshot and swaps it in, so readers never observe a partially-rebuilt state.
type snapshot struct {
	bySource map[string]sourceData
	best     map[string]Price
	updated  time.Time
}

// Catalog holds prices grouped by source plus a precedence-merged "best" view.
// The live state is an immutable *snapshot held in an atomic.Pointer; writeMu
// only serializes concurrent SetSource/Load writers as they build-and-swap.
type Catalog struct {
	cur     atomic.Pointer[snapshot]
	writeMu sync.Mutex
}

// load returns the current immutable snapshot.
func (c *Catalog) load() *snapshot { return c.cur.Load() }

// New builds a Catalog seeded with the built-in price list.
func New() *Catalog {
	c := &Catalog{}
	c.cur.Store(&snapshot{bySource: map[string]sourceData{}, best: map[string]Price{}})
	c.SetSource("builtin", PriorityBuiltin, builtinPrices())
	return c
}

// SetSource replaces one source's prices and rebuilds the merged view by
// building a fresh immutable snapshot and swapping it in atomically.
func (c *Catalog) SetSource(name string, priority int, prices map[string]Price) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	old := c.load()
	bySource := make(map[string]sourceData, len(old.bySource)+1)
	for n, sd := range old.bySource {
		bySource[n] = sd
	}
	bySource[name] = sourceData{Priority: priority, Prices: prices}
	c.cur.Store(&snapshot{
		bySource: bySource,
		best:     buildBest(bySource),
		updated:  time.Now(),
	})
}

// buildBest computes the merged "best" map by filling from sources in ascending
// priority order (lowest priority number wins). Each entry is also indexed by
// its prefix-stripped form so "gpt-4o" and "openai/gpt-4o" both resolve.
func buildBest(bySource map[string]sourceData) map[string]Price {
	names := make([]string, 0, len(bySource))
	total := 0
	for n, sd := range bySource {
		names = append(names, n)
		total += len(sd.Prices)
	}
	sort.Slice(names, func(i, j int) bool {
		pi, pj := bySource[names[i]].Priority, bySource[names[j]].Priority
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	best := make(map[string]Price, 2*total)
	for _, n := range names {
		for id, p := range bySource[n].Prices {
			if _, ok := best[id]; !ok {
				best[id] = p
			}
			if _, rest, found := strings.Cut(id, "/"); found {
				if _, ok := best[rest]; !ok {
					best[rest] = p
				}
			}
		}
	}
	return best
}

// getFrom looks up a model in a price map, trying the exact id then the
// prefix-stripped form.
func getFrom(m map[string]Price, model string) (Price, bool) {
	if p, ok := m[model]; ok {
		return p, true
	}
	if _, rest, found := strings.Cut(model, "/"); found {
		if p, ok := m[rest]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// Get returns the precedence-merged price for a model.
func (c *Catalog) Get(model string) (Price, bool) {
	return getFrom(c.load().best, model)
}

// Rate returns input/output per-MTok rates from the merged view (used by the
// router for least-cost selection).
func (c *Catalog) Rate(model string) (in, out float64, ok bool) {
	p, found := c.Get(model)
	if !found {
		return 0, 0, false
	}
	return p.InputPerMTok, p.OutputPerMTok, true
}

// priceForRoute resolves the price to charge for a model on a given provider
// route: manual overrides win, then the routed provider's own source (e.g.
// "openrouter" margin-inclusive price), then the merged direct-best.
func (c *Catalog) priceForRoute(model, provider string) (Price, bool) {
	snap := c.load()
	if sd, ok := snap.bySource[SourceNameOverride]; ok {
		if p, ok := getFrom(sd.Prices, model); ok {
			return p, true
		}
	}
	if provider != "" {
		if sd, ok := snap.bySource[provider]; ok {
			if p, ok := getFrom(sd.Prices, model); ok {
				return p, true
			}
		}
	}
	return getFrom(snap.best, model)
}

// HasPrice reports whether the catalog can price model on the given provider
// route (same resolution as priceForRoute, price discarded). The metering guard
// uses it to refuse an unpriceable request against a budget-enforcing key BEFORE
// any upstream spend: a served request whose cost cannot be computed would never
// decrement the budget, so a budgeted key could burn unbounded real provider
// spend — the same fail-open class as an unchecked budget on a store error.
// provider "" checks the merged best.
func (c *Catalog) HasPrice(model, provider string) bool {
	_, ok := c.priceForRoute(model, provider)
	return ok
}

// Cost computes the dollar cost of a usage record for a model on a route. The
// provider is the name of the provider actually used (""=unknown, uses best).
func (c *Catalog) Cost(model, provider string, usage *openai.Usage) *openai.Cost {
	if usage == nil {
		return nil
	}
	p, ok := c.priceForRoute(model, provider)
	if !ok {
		return nil
	}
	// Charge cached (prompt-cache hit) tokens at the discounted cache-read rate
	// when both the model price and the usage report them; the rest at full input.
	prompt := usage.PromptTokens
	var in float64
	if d := usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 && p.CacheReadPerMTok > 0 {
		cached := d.CachedTokens
		if cached > prompt {
			cached = prompt
		}
		in = float64(prompt-cached)/1e6*p.InputPerMTok + float64(cached)/1e6*p.CacheReadPerMTok
	} else {
		in = float64(prompt) / 1e6 * p.InputPerMTok
	}
	out := float64(usage.CompletionTokens) / 1e6 * p.OutputPerMTok
	return &openai.Cost{InputCost: in, OutputCost: out, TotalCost: in + out, Currency: "USD"}
}

// Models renders the merged catalog as an OpenAI-style model list.
func (c *Catalog) Models() []openai.Model {
	snap := c.load()
	created := snap.updated.Unix()
	if created <= 0 {
		created = 1
	}
	seen := map[string]bool{}
	out := make([]openai.Model, 0, len(snap.best))
	for id, p := range snap.best {
		if p.Model != "" && p.Model != id {
			continue // skip prefix-stripped duplicate
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, openai.Model{
			ID: id, Object: "model", Created: created, OwnedBy: p.Provider, Provider: p.Provider,
			ContextWindow: p.ContextWindow, MaxOutput: p.MaxOutput,
			InputPrice: p.InputPerMTok, OutputPrice: p.OutputPerMTok,
			Capabilities: p.Capabilities,
		})
	}
	return out
}

// Snapshot returns the merged catalog as a flat model->price map (for export at
// /v1/catalog.json).
func (c *Catalog) Snapshot() map[string]Price {
	best := c.load().best
	out := make(map[string]Price, len(best))
	for id, p := range best {
		if p.Model != "" && p.Model != id {
			continue
		}
		out[id] = p
	}
	return out
}

// Save persists the per-source catalog to disk for warm starts.
func (c *Catalog) Save(path string) error {
	data, err := json.MarshalIndent(c.load().bySource, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load restores a previously-saved catalog (called at startup before sync).
func (c *Catalog) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var loaded map[string]sourceData
	if err := json.Unmarshal(data, &loaded); err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	old := c.load()
	bySource := make(map[string]sourceData, len(old.bySource)+len(loaded))
	for n, sd := range old.bySource {
		bySource[n] = sd
	}
	for name, sd := range loaded {
		if name == "builtin" {
			continue // keep the in-binary seed
		}
		bySource[name] = sd
	}
	c.cur.Store(&snapshot{
		bySource: bySource,
		best:     buildBest(bySource),
		updated:  time.Now(),
	})
	return nil
}

// Updated returns when the catalog last changed.
func (c *Catalog) Updated() time.Time {
	return c.load().updated
}

// Len returns the number of entries in the merged view.
func (c *Catalog) Len() int {
	return len(c.load().best)
}

// builtinPrices is a small offline seed (USD per MTok) so cost works before any
// network sync. Sync overrides these with live data.
func builtinPrices() map[string]Price {
	return map[string]Price{
		"openai/gpt-4o":               {Model: "openai/gpt-4o", Provider: "openai", InputPerMTok: 2.5, OutputPerMTok: 10, ContextWindow: 128000, Capabilities: []string{"tools", "vision"}},
		"openai/gpt-4o-mini":          {Model: "openai/gpt-4o-mini", Provider: "openai", InputPerMTok: 0.15, OutputPerMTok: 0.6, ContextWindow: 128000, Capabilities: []string{"tools", "vision"}},
		"anthropic/claude-3-5-sonnet": {Model: "anthropic/claude-3-5-sonnet", Provider: "anthropic", InputPerMTok: 3, OutputPerMTok: 15, ContextWindow: 200000, Capabilities: []string{"tools", "vision"}},
		"anthropic/claude-3-5-haiku":  {Model: "anthropic/claude-3-5-haiku", Provider: "anthropic", InputPerMTok: 0.8, OutputPerMTok: 4, ContextWindow: 200000, Capabilities: []string{"tools"}},
		"google/gemini-1.5-pro":       {Model: "google/gemini-1.5-pro", Provider: "google", InputPerMTok: 1.25, OutputPerMTok: 5, ContextWindow: 2000000, Capabilities: []string{"tools", "vision"}},
		"google/gemini-1.5-flash":     {Model: "google/gemini-1.5-flash", Provider: "google", InputPerMTok: 0.075, OutputPerMTok: 0.3, ContextWindow: 1000000, Capabilities: []string{"tools", "vision"}},
		"deepseek/deepseek-chat":      {Model: "deepseek/deepseek-chat", Provider: "deepseek", InputPerMTok: 0.28, OutputPerMTok: 0.42, CacheReadPerMTok: 0.028, ContextWindow: 64000, Capabilities: []string{"tools"}},
	}
}

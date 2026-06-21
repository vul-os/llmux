package pricing

import (
	"fmt"
	"sync"
	"testing"

	"github.com/llmux/llmux/core/openai"
)

func TestParseOpenRouter(t *testing.T) {
	data := []byte(`{"data":[
		{"id":"openai/gpt-4o","context_length":128000,"pricing":{"prompt":"0.0000025","completion":"0.00001"},"top_provider":{"max_completion_tokens":16384},"architecture":{"modality":"text+image->text"},"supported_parameters":["tools"]}
	]}`)
	prices, err := ParseOpenRouter(data)
	if err != nil {
		t.Fatal(err)
	}
	p := prices["openai/gpt-4o"]
	if p.InputPerMTok != 2.5 || p.OutputPerMTok != 10 {
		t.Fatalf("rates=%+v", p)
	}
	if p.ContextWindow != 128000 || p.MaxOutput != 16384 {
		t.Fatalf("limits=%+v", p)
	}
	if p.Provider != "openai" {
		t.Fatalf("provider=%q", p.Provider)
	}
	hasTools, hasVision := false, false
	for _, c := range p.Capabilities {
		if c == "tools" {
			hasTools = true
		}
		if c == "vision" {
			hasVision = true
		}
	}
	if !hasTools || !hasVision {
		t.Fatalf("caps=%v", p.Capabilities)
	}
}

func TestParseLiteLLM(t *testing.T) {
	data := []byte(`{
		"sample_spec":{"input_cost_per_token":0},
		"gpt-4o":{"max_input_tokens":128000,"max_output_tokens":16384,"input_cost_per_token":0.0000025,"output_cost_per_token":0.00001,"litellm_provider":"openai","mode":"chat","supports_function_calling":true,"supports_vision":true},
		"text-embedding-3-small":{"input_cost_per_token":0.00000002,"litellm_provider":"openai","mode":"embedding"}
	}`)
	prices, err := ParseLiteLLM(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := prices["sample_spec"]; ok {
		t.Fatal("sample_spec should be skipped")
	}
	if _, ok := prices["text-embedding-3-small"]; ok {
		t.Fatal("embedding mode should be skipped for chat catalog")
	}
	p := prices["gpt-4o"]
	if p.InputPerMTok != 2.5 || p.OutputPerMTok != 10 {
		t.Fatalf("rates=%+v", p)
	}
}

func TestCost(t *testing.T) {
	c := New()
	// 1M prompt + 1M completion of gpt-4o (2.5 in / 10 out) = 12.5 USD.
	cost := c.Cost("openai/gpt-4o", "", &openai.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if cost == nil {
		t.Fatal("expected cost")
	}
	if cost.InputCost != 2.5 || cost.OutputCost != 10 || cost.TotalCost != 12.5 {
		t.Fatalf("cost=%+v", cost)
	}
	if cost.Currency != "USD" {
		t.Fatalf("currency=%q", cost.Currency)
	}
}

func TestCostPrefixStripping(t *testing.T) {
	c := New()
	// "gpt-4o" (no prefix) should resolve via the indexed prefix-stripped key.
	if _, _, ok := c.Rate("gpt-4o"); !ok {
		t.Fatal("expected gpt-4o to resolve from openai/gpt-4o")
	}
}

func TestSetSourceFillsGaps(t *testing.T) {
	c := New()
	before := c.Len()
	c.SetSource("litellm", PriorityLiteLLM, map[string]Price{"custom/model-x": {Model: "custom/model-x", InputPerMTok: 1, OutputPerMTok: 2}})
	if c.Len() <= before {
		t.Fatal("adding a source should add entries")
	}
	if _, _, ok := c.Rate("custom/model-x"); !ok {
		t.Fatal("source model not found")
	}
	// Built-in seed still present.
	if _, _, ok := c.Rate("openai/gpt-4o"); !ok {
		t.Fatal("built-in seed lost")
	}
}

func TestCostCachedTokens(t *testing.T) {
	c := New()
	c.SetSource("override", PriorityOverride, map[string]Price{
		"m": {Model: "m", InputPerMTok: 10, CacheReadPerMTok: 1},
	})
	// 1M prompt of which 600k are cache hits: 0.4M@10 + 0.6M@1 = 4 + 0.6 = 4.6
	cost := c.Cost("m", "", &openai.Usage{PromptTokens: 1_000_000,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 600_000}})
	if cost.InputCost != 4.6 {
		t.Fatalf("cached input cost=%v, want 4.6", cost.InputCost)
	}

	// No cache-read rate configured -> cached tokens billed at full input rate.
	c.SetSource("override", PriorityOverride, map[string]Price{"m": {Model: "m", InputPerMTok: 10}})
	cost = c.Cost("m", "", &openai.Usage{PromptTokens: 1_000_000,
		PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 600_000}})
	if cost.InputCost != 10 {
		t.Fatalf("input cost=%v, want 10 (no discount without cache rate)", cost.InputCost)
	}
}

func TestParseLiteLLMCacheRead(t *testing.T) {
	data := []byte(`{"m":{"input_cost_per_token":0.000003,"output_cost_per_token":0.000015,"cache_read_input_token_cost":0.0000003,"litellm_provider":"anthropic","mode":"chat"}}`)
	prices, err := ParseLiteLLM(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := prices["m"].CacheReadPerMTok; got != 0.3 {
		t.Fatalf("cache_read per-MTok=%v, want 0.3", got)
	}
}

// TestCatalogConcurrentCostAndSetSource runs reads (Cost/Rate/Get) concurrently
// with writers (SetSource) under -race. Each result must be either absent or a
// fully-valid (non-torn) price drawn from a single coherent snapshot.
func TestCatalogConcurrentCostAndSetSource(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	const readers, writers, iters = 32, 8, 500

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				cost := c.Cost("openai/gpt-4o", "", &openai.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
				if cost != nil && cost.TotalCost != cost.InputCost+cost.OutputCost {
					t.Errorf("torn cost: %+v", cost)
					return
				}
				if in, out, ok := c.Rate("openai/gpt-4o"); ok && (in < 0 || out < 0) {
					t.Errorf("torn rate: in=%v out=%v", in, out)
					return
				}
				_, _ = c.Get("anthropic/claude-3-5-sonnet")
				_ = c.Len()
				_ = c.Models()
				_ = c.Snapshot()
			}
		}()
	}
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			name := fmt.Sprintf("src-%d", w)
			for i := 0; i < iters; i++ {
				c.SetSource(name, PriorityLiteLLM, map[string]Price{
					fmt.Sprintf("custom/m-%d", w): {Model: fmt.Sprintf("custom/m-%d", w), InputPerMTok: 1, OutputPerMTok: 2},
				})
			}
		}(w)
	}
	wg.Wait()

	// Built-in seed must survive concurrent writes.
	if _, _, ok := c.Rate("openai/gpt-4o"); !ok {
		t.Fatal("built-in seed lost after concurrent SetSource")
	}
}

func TestUnknownModelNoCost(t *testing.T) {
	c := New()
	if c.Cost("totally-unknown-model", "", &openai.Usage{PromptTokens: 100}) != nil {
		t.Fatal("unknown model should have nil cost")
	}
}

package pricing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/llmux/llmux/core/openai"
)

func mtok(n int) *openai.Usage {
	return &openai.Usage{PromptTokens: n, CompletionTokens: n, TotalTokens: 2 * n}
}

func TestRouteAwarePrecedence(t *testing.T) {
	c := New()
	// LiteLLM (direct) says model "x" is cheap; OpenRouter (margin) says pricier.
	c.SetSource("litellm", PriorityLiteLLM, map[string]Price{
		"x": {Model: "x", InputPerMTok: 1, OutputPerMTok: 1},
	})
	c.SetSource("openrouter", PriorityOpenRouter, map[string]Price{
		"x": {Model: "x", InputPerMTok: 2, OutputPerMTok: 2},
	})

	// Direct route (no/!openrouter provider) -> cheaper LiteLLM price wins.
	direct := c.Cost("x", "openai", mtok(1_000_000))
	if direct.TotalCost != 2 { // (1+1) per MTok * 1M each
		t.Fatalf("direct total=%v, want 2 (LiteLLM)", direct.TotalCost)
	}
	// Routed through OpenRouter -> OpenRouter's margin-inclusive price applies.
	viaOR := c.Cost("x", "openrouter", mtok(1_000_000))
	if viaOR.TotalCost != 4 { // (2+2)
		t.Fatalf("openrouter total=%v, want 4", viaOR.TotalCost)
	}
}

func TestOverrideAlwaysWins(t *testing.T) {
	c := New()
	c.SetSource("openrouter", PriorityOpenRouter, map[string]Price{
		"x": {Model: "x", InputPerMTok: 99, OutputPerMTok: 99},
	})
	c.SetSource(SourceNameOverride, PriorityOverride, map[string]Price{
		"x": {Model: "x", InputPerMTok: 1, OutputPerMTok: 1},
	})
	// Even when routed through openrouter, a manual override wins.
	cost := c.Cost("x", "openrouter", mtok(1_000_000))
	if cost.TotalCost != 2 {
		t.Fatalf("override total=%v, want 2", cost.TotalCost)
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	c1 := New()
	c1.SetSource("litellm", PriorityLiteLLM, map[string]Price{
		"custom/m": {Model: "custom/m", InputPerMTok: 3, OutputPerMTok: 4},
	})
	if err := c1.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("catalog not written: %v", err)
	}

	c2 := New()
	if err := c2.Load(path); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c2.Rate("custom/m"); !ok {
		t.Fatal("loaded catalog missing synced model")
	}
	// Built-in seed should still be present after Load.
	if _, _, ok := c2.Rate("openai/gpt-4o"); !ok {
		t.Fatal("built-in seed lost after load")
	}
}

func TestParseAzure(t *testing.T) {
	data := []byte(`{"Items":[
		{"unitPrice":0.0025,"productName":"Azure OpenAI","meterName":"gpt-4o Input Global Tokens","unitOfMeasure":"1K"},
		{"unitPrice":0.01,"productName":"Azure OpenAI","meterName":"gpt-4o Output Global Tokens","unitOfMeasure":"1K"},
		{"unitPrice":5.0,"productName":"Azure OpenAI","meterName":"DALL-E Images","unitOfMeasure":"100"}
	]}`)
	prices, err := ParseAzure(data)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := prices["azure/gpt-4o"]
	if !ok {
		t.Fatalf("azure/gpt-4o missing: %+v", prices)
	}
	// 0.0025 per 1K tokens -> 2.5 per MTok; 0.01 -> 10.
	if p.InputPerMTok != 2.5 || p.OutputPerMTok != 10 {
		t.Fatalf("azure rates=%+v", p)
	}
	// Non-token meter (images) must be skipped.
	if _, ok := prices["azure/dall-e-images"]; ok {
		t.Fatal("image meter should be skipped")
	}
}

func TestSnapshotExcludesPrefixDupes(t *testing.T) {
	c := New()
	snap := c.Snapshot()
	// "openai/gpt-4o" present, prefix-stripped "gpt-4o" excluded from export.
	if _, ok := snap["openai/gpt-4o"]; !ok {
		t.Fatal("expected canonical id in snapshot")
	}
	if _, ok := snap["gpt-4o"]; ok {
		t.Fatal("prefix-stripped dupe should be excluded from snapshot")
	}
}

func TestSourceFromURL(t *testing.T) {
	if NewOpenRouterSource("").Name() != "openrouter" || NewOpenRouterSource("").Priority() != PriorityOpenRouter {
		t.Fatal("openrouter source misconfigured")
	}
	if SourceFromURL("https://openrouter.ai/api/v1/models").Name() != "openrouter" {
		t.Fatal("URL detection failed for openrouter")
	}
	if SourceFromURL("https://x/model_prices_and_context_window.json").Name() != "litellm" {
		t.Fatal("URL detection failed for litellm")
	}
}

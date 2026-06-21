package router

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// fakeProvider is a no-op provider for routing tests.
type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) ChatCompletion(context.Context, *openai.ChatCompletionRequest, string, json.RawMessage) (*openai.ChatCompletionResponse, error) {
	return nil, nil
}
func (f *fakeProvider) ChatCompletionStream(context.Context, *openai.ChatCompletionRequest, string, json.RawMessage, provider.ChunkFunc) error {
	return nil
}
func (f *fakeProvider) Embeddings(context.Context, *openai.EmbeddingRequest, string, json.RawMessage) (*openai.EmbeddingResponse, error) {
	return nil, nil
}

func regWith(names ...string) *provider.Registry {
	r := provider.NewRegistry()
	for _, n := range names {
		r.Register(&fakeProvider{n})
	}
	return r
}

// fakePricer returns fixed rates.
type fakePricer map[string]float64

func (f fakePricer) Rate(model string) (in, out float64, ok bool) {
	v, ok := f[model]
	return v, v, ok
}

func TestExactBeatsCatchAll(t *testing.T) {
	reg := regWith("a", "b")
	r := New([]config.RouteConfig{
		{Model: "*", Provider: "a"},
		{Model: "gpt", Provider: "b", TargetModel: "gpt-up"},
	}, reg, nil)
	res, err := r.Resolve("gpt")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Provider.Name() != "b" || res.Primary.Model != "gpt-up" {
		t.Fatalf("got %s/%s", res.Primary.Provider.Name(), res.Primary.Model)
	}
}

func TestCatchAllForwardsModel(t *testing.T) {
	reg := regWith("a")
	r := New([]config.RouteConfig{{Model: "*", Provider: "a"}}, reg, nil)
	res, _ := r.Resolve("some-model")
	if res.Primary.Model != "some-model" {
		t.Fatalf("catch-all should forward requested model, got %q", res.Primary.Model)
	}
}

func TestPrefixSyntax(t *testing.T) {
	reg := regWith("openai")
	r := New(nil, reg, nil)
	res, err := r.Resolve("openai/gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Provider.Name() != "openai" || res.Primary.Model != "gpt-4o" {
		t.Fatalf("got %s/%s", res.Primary.Provider.Name(), res.Primary.Model)
	}
}

func TestUnknownModelErrors(t *testing.T) {
	r := New(nil, regWith("a"), nil)
	if _, err := r.Resolve("nope"); err == nil {
		t.Fatal("expected error for unroutable model")
	}
}

func TestPrefixBeatsCatchAll(t *testing.T) {
	reg := regWith("anthropic", "fallthrough")
	r := New([]config.RouteConfig{
		{Model: "*", Provider: "fallthrough"},
		{Model: "claude-*", Provider: "anthropic"},
	}, reg, nil)
	res, err := r.Resolve("claude-3-opus")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Provider.Name() != "anthropic" {
		t.Fatalf("prefix should beat catch-all, got %s", res.Primary.Provider.Name())
	}
	// No TargetModel: forward the full requested model.
	if res.Primary.Model != "claude-3-opus" {
		t.Fatalf("empty target should forward full model, got %q", res.Primary.Model)
	}
}

func TestExactBeatsPrefix(t *testing.T) {
	reg := regWith("exact", "pref")
	r := New([]config.RouteConfig{
		{Model: "claude-*", Provider: "pref", TargetModel: "pref-model"},
		{Model: "claude-3-opus", Provider: "exact", TargetModel: "exact-model"},
	}, reg, nil)
	res, err := r.Resolve("claude-3-opus")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Provider.Name() != "exact" || res.Primary.Model != "exact-model" {
		t.Fatalf("exact should beat prefix, got %s/%s", res.Primary.Provider.Name(), res.Primary.Model)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	reg := regWith("short", "long")
	r := New([]config.RouteConfig{
		{Model: "gpt-*", Provider: "short", TargetModel: "short-model"},
		{Model: "gpt-4*", Provider: "long", TargetModel: "long-model"},
	}, reg, nil)
	res, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Provider.Name() != "long" || res.Primary.Model != "long-model" {
		t.Fatalf("longest prefix should win, got %s/%s", res.Primary.Provider.Name(), res.Primary.Model)
	}
}

func TestPrefixTargetSubstitution(t *testing.T) {
	reg := regWith("azure")
	r := New([]config.RouteConfig{
		{Model: "gpt-4*", Provider: "azure", TargetModel: "azure/gpt-4*"},
	}, reg, nil)
	res, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Model != "azure/gpt-4o" {
		t.Fatalf("target substitution failed, got %q want azure/gpt-4o", res.Primary.Model)
	}
}

func TestPrefixTargetLiteralNoStar(t *testing.T) {
	reg := regWith("anthropic")
	r := New([]config.RouteConfig{
		{Model: "claude-*", Provider: "anthropic", TargetModel: "claude-3-5-sonnet"},
	}, reg, nil)
	res, err := r.Resolve("claude-anything")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Model != "claude-3-5-sonnet" {
		t.Fatalf("literal target should be used as-is, got %q", res.Primary.Model)
	}
}

func TestLeastCostOrdersCheapestFirst(t *testing.T) {
	reg := regWith("cheap", "pricey")
	pricer := fakePricer{"cheap-model": 1.0, "pricey-model": 50.0}
	r := New([]config.RouteConfig{{
		Model: "auto", Strategy: "least-cost",
		Candidates: []config.Candidate{
			{Provider: "pricey", Model: "pricey-model"},
			{Provider: "cheap", Model: "cheap-model"},
		},
	}}, reg, pricer)

	res, err := r.Resolve("auto")
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Provider.Name() != "cheap" {
		t.Fatalf("primary=%s, want cheap", res.Primary.Provider.Name())
	}
	if len(res.Fallbacks) != 1 || res.Fallbacks[0].Provider.Name() != "pricey" {
		t.Fatalf("fallbacks=%+v", res.Fallbacks)
	}
}

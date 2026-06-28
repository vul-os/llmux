package router

import (
	"testing"

	"github.com/llmux/llmux/core/config"
)

// ---------------------------------------------------------------------------
// Resolution.All() ordering
// ---------------------------------------------------------------------------

// TestResolutionAllReturnsPrimaryFirst verifies that All() returns the primary
// target at index 0 followed by fallbacks in order — this is the order
// dispatchUnary iterates for fan-out.
func TestResolutionAllReturnsPrimaryFirst(t *testing.T) {
	reg := regWith("a", "b", "c")
	r := New([]config.RouteConfig{
		{Model: "m", Provider: "a", Fallbacks: []string{"b", "c"}},
	}, reg, nil)
	res, err := r.Resolve("m")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	all := res.All()
	if len(all) != 3 {
		t.Fatalf("All() len=%d, want 3", len(all))
	}
	if all[0].Provider.Name() != "a" {
		t.Fatalf("All()[0]=%s, want a (primary)", all[0].Provider.Name())
	}
	if all[1].Provider.Name() != "b" {
		t.Fatalf("All()[1]=%s, want b (first fallback)", all[1].Provider.Name())
	}
	if all[2].Provider.Name() != "c" {
		t.Fatalf("All()[2]=%s, want c (second fallback)", all[2].Provider.Name())
	}
}

// TestResolutionAllNoPrimaryOnlyReturnsOne verifies that when there are no
// fallbacks, All() returns a single-element slice (the primary only).
func TestResolutionAllNoPrimaryOnlyReturnsOne(t *testing.T) {
	reg := regWith("p")
	r := New([]config.RouteConfig{{Model: "*", Provider: "p"}}, reg, nil)
	res, err := r.Resolve("any")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	all := res.All()
	if len(all) != 1 {
		t.Fatalf("All() len=%d, want 1 (no fallbacks)", len(all))
	}
}

// TestFallbackOnlyValidProvidersAppended verifies that a route with a
// non-existent fallback provider name silently skips it — only registered
// providers end up in the fallback slice. This prevents a mis-configuration
// from crashing the gateway.
func TestFallbackOnlyValidProvidersAppended(t *testing.T) {
	reg := regWith("a", "c") // "b" is NOT registered
	r := New([]config.RouteConfig{
		{Model: "m", Provider: "a", Fallbacks: []string{"b", "c"}},
	}, reg, nil)
	res, err := r.Resolve("m")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Fallbacks) != 1 {
		t.Fatalf("fallbacks=%d, want 1 (unregistered 'b' skipped)", len(res.Fallbacks))
	}
	if res.Fallbacks[0].Provider.Name() != "c" {
		t.Fatalf("fallback[0]=%s, want c", res.Fallbacks[0].Provider.Name())
	}
}

// ---------------------------------------------------------------------------
// Least-cost routing edge cases
// ---------------------------------------------------------------------------

// TestLeastCostUnknownPricingSortsLast verifies that a candidate without a
// known price is sorted AFTER any candidate with a known price. An unknown
// price means a cost of 1e18, which should lose to any real price.
func TestLeastCostUnknownPricingSortsLast(t *testing.T) {
	reg := regWith("cheap", "unknown")
	pricer := fakePricer{"cheap-model": 0.5} // "unknown-model" has no entry
	r := New([]config.RouteConfig{{
		Model: "auto", Strategy: "least-cost",
		Candidates: []config.Candidate{
			{Provider: "unknown", Model: "unknown-model"},
			{Provider: "cheap", Model: "cheap-model"},
		},
	}}, reg, pricer)

	res, err := r.Resolve("auto")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Primary.Provider.Name() != "cheap" {
		t.Fatalf("primary=%s, want cheap (known price beats unknown)", res.Primary.Provider.Name())
	}
	if res.Fallbacks[0].Provider.Name() != "unknown" {
		t.Fatalf("fallback[0]=%s, want unknown", res.Fallbacks[0].Provider.Name())
	}
}

// TestLeastCostAllUnknownPricing verifies that least-cost routing still works
// when NONE of the candidates have pricing data: any one of them must be
// selected as primary (no error, no panic).
func TestLeastCostAllUnknownPricing(t *testing.T) {
	reg := regWith("a", "b")
	r := New([]config.RouteConfig{{
		Model: "auto", Strategy: "least-cost",
		Candidates: []config.Candidate{
			{Provider: "a", Model: "a-model"},
			{Provider: "b", Model: "b-model"},
		},
	}}, reg, nil) // nil pricer → no pricing data at all

	res, err := r.Resolve("auto")
	if err != nil {
		t.Fatalf("resolve: %v (want success even without pricing)", err)
	}
	// Both cost 1e18; stable sort must preserve declaration order.
	if res.Primary.Provider.Name() != "a" {
		t.Fatalf("primary=%s, want a (stable sort on tie, declared first)", res.Primary.Provider.Name())
	}
}

// TestLeastCostNoCandidatesResolveToRegisteredProvider verifies that a
// least-cost route whose candidate providers are ALL unregistered returns an
// error rather than panicking or routing to a phantom provider.
func TestLeastCostNoCandidatesResolveToRegisteredProvider(t *testing.T) {
	reg := regWith("real") // neither "ghost1" nor "ghost2" is registered
	r := New([]config.RouteConfig{{
		Model: "auto", Strategy: "least-cost",
		Candidates: []config.Candidate{
			{Provider: "ghost1", Model: "m1"},
			{Provider: "ghost2", Model: "m2"},
		},
	}}, reg, nil)

	if _, err := r.Resolve("auto"); err == nil {
		t.Fatal("expected error for least-cost route with no registered candidates")
	}
}

// TestLeastCostTieBreakStable verifies that when two candidates have the exact
// same price, the stable sort preserves their declaration order: the first
// declared candidate wins the primary slot.
func TestLeastCostTieBreakStable(t *testing.T) {
	reg := regWith("first", "second")
	price := 1.0
	pricer := fakePricer{"m1": price, "m2": price}
	r := New([]config.RouteConfig{{
		Model: "auto", Strategy: "least-cost",
		Candidates: []config.Candidate{
			{Provider: "first", Model: "m1"},
			{Provider: "second", Model: "m2"},
		},
	}}, reg, pricer)

	res, err := r.Resolve("auto")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Primary.Provider.Name() != "first" {
		t.Fatalf("primary=%s, want first (stable tie-break by declaration order)", res.Primary.Provider.Name())
	}
}

// ---------------------------------------------------------------------------
// Prefix routing with model name propagation
// ---------------------------------------------------------------------------

// TestPrefixCatchAllFallsThrough verifies that a model that doesn't match any
// specific prefix routes falls through to the catch-all.
func TestPrefixCatchAllFallsThrough(t *testing.T) {
	reg := regWith("specific", "fallback")
	r := New([]config.RouteConfig{
		{Model: "claude-*", Provider: "specific"},
		{Model: "*", Provider: "fallback"},
	}, reg, nil)

	res, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Primary.Provider.Name() != "fallback" {
		t.Fatalf("provider=%s, want fallback", res.Primary.Provider.Name())
	}
	if res.Primary.Model != "gpt-4o" {
		t.Fatalf("model=%q, want gpt-4o (catch-all forwards full model)", res.Primary.Model)
	}
}

// TestProviderPrefixSyntaxDoesNotAllowUnregistered verifies that the
// provider/model prefix syntax only works for REGISTERED providers. An
// attacker cannot inject an arbitrary provider name to cause an outbound
// request to an unregistered (and presumably unconfigured) endpoint.
func TestProviderPrefixSyntaxDoesNotAllowUnregistered(t *testing.T) {
	reg := regWith("openai") // "anthropic" is not registered
	r := New(nil, reg, nil)

	if _, err := r.Resolve("anthropic/claude-3-opus"); err == nil {
		t.Fatal("prefix syntax for unregistered provider must error, not route")
	}
}

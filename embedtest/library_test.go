package embedtest

import (
	"context"
	"testing"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
)

// G1 — the internal/ wall is really gone.
//
// This file's existence is half the guard (a separate module can only reach
// exported API), but a compile-only test would pass while the library did
// nothing. So it drives a real request end to end through the public surface
// and asserts every field of Result that an in-process caller cannot get from
// the HTTP shell: which provider actually served, whether it was BYOK, whether
// the cache answered, the relayed upstream headers, and the serialized body.
func TestLibraryChatThroughPublicAPI(t *testing.T) {
	up := newFakeUpstream(t, "hello from the fake upstream")

	cfg := &config.Config{
		Providers: []config.ProviderConfig{up.providerCfg("fake", "central-key", "")},
		Routes:    []config.RouteConfig{{Model: "demo", Provider: "fake", TargetModel: "upstream-model"}},
	}

	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	req := &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("ping")}},
	}
	res, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if res.Provider != "fake" {
		t.Errorf("Result.Provider = %q, want %q — the caller cannot tell who served", res.Provider, "fake")
	}
	if res.BYOK {
		t.Error("Result.BYOK = true on a request served with the central key")
	}
	if res.CacheHit {
		t.Error("Result.CacheHit = true on the first request, with caching disabled")
	}
	if res.Response == nil || len(res.Response.Choices) == 0 {
		t.Fatalf("Result.Response has no choices: %+v", res.Response)
	}
	if got := res.Response.Choices[0].Message.Content.String(); got != "hello from the fake upstream" {
		t.Errorf("content = %q, want the fake upstream's answer", got)
	}
	if got := res.Response.Model; got != "upstream-model" {
		t.Errorf("upstream saw model %q, want the routed target %q", got, "upstream-model")
	}
	if len(res.Body) == 0 {
		t.Error("Result.Body is empty — the HTTP shell writes these bytes verbatim, so nothing would be served")
	}
	if got := res.Headers.Get("X-Ratelimit-Remaining-Requests"); got != "41" {
		t.Errorf("Result.Headers dropped the upstream rate-limit relay: got %q, want %q", got, "41")
	}
	if up.Hits() != 1 {
		t.Errorf("upstream saw %d requests, want exactly 1", up.Hits())
	}
}

// The cache path is part of the library surface (Result.CacheHit), and a
// CacheHit field that is never true is indistinguishable from a stub. Prove the
// second identical request is served without touching the upstream.
func TestLibraryCacheHitIsObservable(t *testing.T) {
	up := newFakeUpstream(t, "cached answer")

	cfg := &config.Config{
		Providers: []config.ProviderConfig{up.providerCfg("fake", "central-key", "")},
		Routes:    []config.RouteConfig{{Model: "demo", Provider: "fake"}},
		Cache:     config.CacheConfig{Enabled: true, MaxEntries: 16},
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	req := &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("same question")}},
	}
	first, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if first.CacheHit {
		t.Fatal("first request reported CacheHit — nothing was in the cache yet")
	}
	second, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if !second.CacheHit {
		t.Error("second identical request did not report CacheHit")
	}
	if up.Hits() != 1 {
		t.Errorf("upstream saw %d requests, want 1 — the cache did not spare the second call", up.Hits())
	}
}

// Authorize is the library's auth entry point and it hands back a release
// func the caller MUST call. It is documented as never nil; assert that,
// because a nil return would make every correct caller panic.
func TestAuthorizeReturnsAReleaseFunc(t *testing.T) {
	up := newFakeUpstream(t, "x")
	cfg := &config.Config{
		Providers: []config.ProviderConfig{up.providerCfg("fake", "k", "")},
		Routes:    []config.RouteConfig{{Model: "demo", Provider: "fake"}},
		Keys:      []config.KeyConfig{{Key: "sk-test", Name: "test"}},
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	ctx, release, err := gw.Authorize(context.Background(), "sk-test")
	if err != nil {
		t.Fatalf("Authorize with a configured key: %v", err)
	}
	if release == nil {
		t.Fatal("Authorize returned a nil release func — every caller does `defer release()` and would panic")
	}
	release()
	if ctx == nil {
		t.Fatal("Authorize returned a nil context")
	}

	if _, release, err := gw.Authorize(context.Background(), "sk-not-a-key"); err == nil {
		t.Error("Authorize accepted an unknown token")
	} else if release == nil {
		t.Error("Authorize returned a nil release func on the DENY path — `defer release()` runs there too")
	}
}

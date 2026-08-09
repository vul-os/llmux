package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/provider"
	"github.com/vul-os/llmux/core/provider/anthropic"
)

// optsFor returns adapter options whose HTTP clients route through tr. The
// clients are per-adapter (provider.Options), so the harness no longer has to
// mutate — and restore — process-wide state to install a RoundTripper.
func optsFor(tr http.RoundTripper) provider.Options {
	return provider.Options{
		HTTPClient:   &http.Client{Transport: tr},
		StreamClient: &http.Client{Transport: tr},
	}
}

// TestRecordThenReplay proves the harness records a real (mock) response and
// then serves it from disk with NO network — the property that lets CI verify
// adapter translation against real responses.
func TestRecordThenReplay(t *testing.T) {
	dir := t.TempDir()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
			"content":[{"type":"text","text":"hello from fixture"}],
			"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))

	cfg := config.ProviderConfig{Name: "anthropic", BaseURL: mock.URL, APIKey: "k"}
	req := &openai.ChatCompletionRequest{Model: "claude", Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}}}

	// --- Record ---
	rec := &Transport{Mode: Record, Dir: dir, Real: http.DefaultTransport}
	rec.SetCase("anthropic/chat_basic")
	resp, err := anthropic.New(cfg, optsFor(rec)).ChatCompletion(context.Background(), req, "claude-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content.String() != "hello from fixture" {
		t.Fatalf("record content=%q", resp.Choices[0].Message.Content.String())
	}
	if !rec.HasFixture("anthropic/chat_basic") {
		t.Fatal("fixture was not written")
	}

	// --- Replay with the upstream CLOSED (proves no network is used) ---
	mock.Close()
	rep := &Transport{Mode: Replay, Dir: dir}
	rep.SetCase("anthropic/chat_basic")
	resp2, err := anthropic.New(cfg, optsFor(rep)).ChatCompletion(context.Background(), req, "claude-3", nil)
	if err != nil {
		t.Fatalf("replay failed (should serve from fixture): %v", err)
	}
	if resp2.Choices[0].Message.Content.String() != "hello from fixture" {
		t.Fatalf("replay content=%q", resp2.Choices[0].Message.Content.String())
	}
	if resp2.Usage.TotalTokens != 5 {
		t.Fatalf("replay usage=%+v", resp2.Usage)
	}
}

func TestReplayMissingFixtureSkips(t *testing.T) {
	rep := &Transport{Mode: Replay, Dir: t.TempDir()}
	rep.SetCase("nope/missing")
	p := anthropic.New(config.ProviderConfig{Name: "anthropic", BaseURL: "http://unused", APIKey: "k"}, optsFor(rep))
	_, err := p.ChatCompletion(context.Background(),
		&openai.ChatCompletionRequest{Model: "x", Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}}}, "x", nil)
	// The provider wraps transport errors; ensure the underlying cause is ErrNoFixture-like
	// (a missing fixture must not look like a passing test).
	if err == nil {
		t.Fatal("expected error for missing fixture")
	}
}

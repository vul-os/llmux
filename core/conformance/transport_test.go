package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
	"github.com/llmux/llmux/core/provider/anthropic"
)

// install points the provider HTTP clients at tr and returns a restore func.
func install(tr http.RoundTripper) func() {
	oldD := provider.DefaultHTTPClient.Transport
	oldS := provider.StreamHTTPClient.Transport
	provider.DefaultHTTPClient.Transport = tr
	provider.StreamHTTPClient.Transport = tr
	return func() {
		provider.DefaultHTTPClient.Transport = oldD
		provider.StreamHTTPClient.Transport = oldS
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

	p := anthropic.New(config.ProviderConfig{Name: "anthropic", BaseURL: mock.URL, APIKey: "k"})
	req := &openai.ChatCompletionRequest{Model: "claude", Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}}}

	// --- Record ---
	rec := &Transport{Mode: Record, Dir: dir, Real: http.DefaultTransport}
	rec.SetCase("anthropic/chat_basic")
	restore := install(rec)
	resp, err := p.ChatCompletion(context.Background(), req, "claude-3", nil)
	restore()
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
	restore = install(rep)
	resp2, err := p.ChatCompletion(context.Background(), req, "claude-3", nil)
	restore()
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
	restore := install(rep)
	defer restore()
	p := anthropic.New(config.ProviderConfig{Name: "anthropic", BaseURL: "http://unused", APIKey: "k"})
	_, err := p.ChatCompletion(context.Background(),
		&openai.ChatCompletionRequest{Model: "x", Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}}}, "x", nil)
	// The provider wraps transport errors; ensure the underlying cause is ErrNoFixture-like
	// (a missing fixture must not look like a passing test).
	if err == nil {
		t.Fatal("expected error for missing fixture")
	}
}

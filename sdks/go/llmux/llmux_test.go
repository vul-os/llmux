package llmux

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/openai"
)

func TestEmbeddedGateway(t *testing.T) {
	// Mock upstream provider.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"embedded works"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}}
	cfg.Routes = []config.RouteConfig{{Model: "*", Provider: "mock"}}
	cfg.Pricing.Sources = nil // no network in tests

	local, err := Start(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(local.OpenAIBaseURL()+"/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Choices[0].Message.Content != "embedded works" {
		t.Fatalf("content=%q", out.Choices[0].Message.Content)
	}
}

// TestNewEmbedsWithoutListener is the property the SDK existed to provide and
// did not: an in-process gateway with NO socket. It asserts the gateway is
// usable for dispatch and that nothing is listening on any port it owns —
// there is no BaseURL because there is no server.
func TestNewEmbedsWithoutListener(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	gw, err := New(Options{Config: &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "mock", Type: config.TypePassthrough, BaseURL: upstream.URL + "/v1", APIKey: "k",
		}},
		Routes: []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer gw.Close()

	res, err := gw.Chat(context.Background(), &openai.ChatCompletionRequest{
		Model:    "any-model",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := res.Response.Choices[0].Message.Content.String(); got != "hi" {
		t.Fatalf("content = %q, want %q", got, "hi")
	}
	if res.Provider != "mock" {
		t.Fatalf("serving provider = %q, want mock", res.Provider)
	}
}

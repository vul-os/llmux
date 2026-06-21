package llmux

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
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

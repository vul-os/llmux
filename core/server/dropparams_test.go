package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// TestDropParams verifies configured params are stripped before forwarding to an
// OpenAI-shaped upstream (and unconfigured params pass through).
func TestDropParams(t *testing.T) {
	var got map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "x", Object: "chat.completion", Model: "m",
			Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: openai.Str("ok")}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()

	cfg := &config.Config{
		Server:     config.ServerConfig{Addr: ":0"},
		DropParams: []string{"logprobs", "logit_bias"},
		Providers:  []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:     []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, _ := New(cfg) // sets provider.DropParams; next New() resets it

	post(s, `{"model":"m","messages":[],"logprobs":true,"logit_bias":{"1":2},"temperature":0.5}`)

	if _, present := got["logprobs"]; present {
		t.Fatal("logprobs should have been dropped")
	}
	if _, present := got["logit_bias"]; present {
		t.Fatal("logit_bias should have been dropped")
	}
	if got["temperature"] != 0.5 {
		t.Fatalf("non-dropped param lost: %v", got["temperature"])
	}
}

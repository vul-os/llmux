package embedtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/vul-os/llmux/core/config"
)

// fakeUpstream is an OpenAI-shaped provider on loopback. Loopback matters: the
// sovereignty policy classifies a loopback base URL as "local", so these tests
// exercise the real dispatch path without needing an AllowEgress opt-in that
// would misrepresent how an embedder actually runs a local model.
type fakeUpstream struct {
	*httptest.Server
	hits    atomic.Int64
	lastKey atomic.Value // string: the Authorization header of the last request
	content string
}

func newFakeUpstream(t *testing.T, content string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{content: content}
	f.lastKey.Store("")
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.lastKey.Store(r.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "41")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-fake",
			"object":  "chat.completion",
			"created": 1,
			"model":   body["model"],
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": f.content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) Hits() int64      { return f.hits.Load() }
func (f *fakeUpstream) LastAuth() string { return f.lastKey.Load().(string) }

// providerCfg is a passthrough provider pointed at the fake upstream.
func (f *fakeUpstream) providerCfg(name, apiKey, apiKeyEnv string) config.ProviderConfig {
	return config.ProviderConfig{
		Name: name, Type: config.TypePassthrough, BaseURL: f.URL,
		APIKey: apiKey, APIKeyEnv: apiKeyEnv,
	}
}

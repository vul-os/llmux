// Package fake is an OpenAI-compatible upstream that answers from memory.
//
// The C smoke test, the Go unit tests and the latency benchmark all need a real
// request path — routing, provider adapter, SSE parsing, metering — without a
// provider account and without the network. They all use this, so the three of
// them exercise the same upstream behaviour and a change to it cannot make one
// of them agree with a fiction the others do not share.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Upstream is a fake OpenAI-compatible server.
type Upstream struct {
	// Text is the assistant content every chat answer carries. Streaming splits
	// it on spaces, one chunk per word, so a stream has more than one frame and
	// a test can prove it was assembled rather than delivered whole.
	Text string

	chatHits   atomic.Int64
	streamHits atomic.Int64
	embedHits  atomic.Int64
}

// New returns an upstream that answers with text.
func New(text string) *Upstream { return &Upstream{Text: text} }

// ChatHits is the number of non-streaming chat requests served.
func (u *Upstream) ChatHits() int64 { return u.chatHits.Load() }

// StreamHits is the number of streaming chat requests served.
func (u *Upstream) StreamHits() int64 { return u.streamHits.Load() }

// EmbedHits is the number of embeddings requests served.
func (u *Upstream) EmbedHits() int64 { return u.embedHits.Load() }

// Handler routes the two endpoints llmux's passthrough provider calls.
func (u *Upstream) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", u.chat)
	mux.HandleFunc("/v1/embeddings", u.embeddings)
	return mux
}

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (u *Upstream) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Stream {
		u.streamHits.Add(1)
		u.streamChat(w, req.Model)
		return
	}
	u.chatHits.Add(1)
	body, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": u.Text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 5, "total_tokens": 12},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (u *Upstream) streamChat(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	frame := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	words := strings.Fields(u.Text)
	for i, word := range words {
		text := word
		if i < len(words)-1 {
			text += " "
		}
		frame(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil,
			}},
		})
	}
	// Terminal frame with a finish reason, then the usage frame llmux asks for
	// via stream_options.include_usage, then [DONE].
	frame(map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}},
	})
	frame(map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 7, "completion_tokens": len(words), "total_tokens": 7 + len(words)},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (u *Upstream) embeddings(w http.ResponseWriter, r *http.Request) {
	u.embedHits.Add(1)
	var req chatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	body, _ := json.Marshal(map[string]any{
		"object": "list",
		"model":  req.Model,
		"data": []any{map[string]any{
			"object": "embedding", "index": 0, "embedding": []float64{0.25, -0.5, 0.75},
		}},
		"usage": map[string]any{"prompt_tokens": 3, "total_tokens": 3},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// ConfigJSON is an llmux configuration document routing model "demo" at this
// upstream. It is the configuration the C smoke test, the unit tests and the
// benchmark all pass to llmux_new, so they configure llmux identically.
func ConfigJSON(baseURL string) string {
	cfg := map[string]any{
		"providers": []any{map[string]any{
			"name": "fake", "type": "passthrough", "base_url": strings.TrimRight(baseURL, "/") + "/v1",
		}},
		"routes": []any{map[string]any{
			"model": "demo", "provider": "fake", "target_model": "upstream-model",
		}},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

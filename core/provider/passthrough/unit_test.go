package passthrough

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

func TestSetJSONFieldsSinglePass(t *testing.T) {
	// model + stream set in one pass; other fields preserved.
	out := provider.SetJSONFields([]byte(`{"model":"a","messages":[],"temperature":0.5}`),
		map[string]any{"model": "b", "stream": false})
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["model"] != "b" || m["temperature"] != 0.5 || m["stream"] != false {
		t.Fatalf("single-pass rewrite wrong: %v", m)
	}
	if !strings.Contains(string(out), `"stream":false`) {
		t.Fatalf("stream not set: %s", out)
	}
	// Empty raw -> object with just the fields.
	out = provider.SetJSONFields(nil, map[string]any{"model": "x"})
	json.Unmarshal(out, &m)
	if m["model"] != "x" {
		t.Fatalf("empty raw model=%v", m["model"])
	}
	// Non-JSON input returned unchanged (no panic).
	if got := provider.SetJSONFields([]byte("not json"), map[string]any{"model": "y"}); string(got) != "not json" {
		t.Fatalf("non-json should be unchanged, got %s", got)
	}
}

func TestStreamParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ensure the request was marked streaming.
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true {
			t.Errorf("upstream did not receive stream=true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, tok := range []string{"He", "llo"} {
			chunk := openai.ChatCompletionChunk{Object: "chat.completion.chunk",
				Choices: []openai.ChunkChoice{{Delta: openai.Delta{Content: tok}}}}
			b, _ := json.Marshal(chunk)
			w.Write([]byte("data: " + string(b) + "\n\n"))
			fl.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer srv.Close()

	p := newP(srv.URL + "/v1")
	var got string
	err := p.ChatCompletionStream(context.Background(), basicReq(), "m", []byte(`{"model":"m"}`),
		func(c *openai.ChatCompletionChunk) error {
			if len(c.Choices) > 0 {
				got += c.Choices[0].Delta.Content
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello" {
		t.Fatalf("streamed=%q", got)
	}
}

func TestEmbeddingsForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			t.Errorf("path=%s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(openai.EmbeddingResponse{Object: "list", Model: "e",
			Data: []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}}}})
	}))
	defer srv.Close()

	p := newP(srv.URL + "/v1")
	in, _ := json.Marshal("hello")
	resp, err := p.Embeddings(context.Background(), &openai.EmbeddingRequest{Model: "e", Input: in}, "e", []byte(`{"model":"e"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 2 {
		t.Fatalf("bad embeddings: %+v", resp)
	}
}

func TestErrorFromResponseStructured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(openai.NewError("slow down", "rate_limit_error", "rate_limited"))
	}))
	defer srv.Close()
	p := newP(srv.URL + "/v1")
	_, err := p.ChatCompletion(context.Background(), basicReq(), "m", []byte(`{"model":"m"}`))
	pe, ok := err.(*provider.Error)
	if !ok {
		t.Fatalf("err type=%T", err)
	}
	if pe.Status() != 429 || pe.Body.Error.Type != "rate_limit_error" {
		t.Fatalf("relayed error wrong: %d %+v", pe.Status(), pe.Body)
	}
}

func TestErrorFromResponseNonJSONNotLeaked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte("<html>internal proxy secrets</html>"))
	}))
	defer srv.Close()
	p := newP(srv.URL + "/v1")
	_, err := p.ChatCompletion(context.Background(), basicReq(), "m", []byte(`{"model":"m"}`))
	pe := err.(*provider.Error)
	if strings.Contains(pe.Body.Error.Message, "internal proxy secrets") {
		t.Fatalf("raw upstream body leaked: %s", pe.Body.Error.Message)
	}
}

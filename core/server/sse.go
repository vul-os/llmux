package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/llmux/llmux/core/openai"
)

// sseWriter writes Server-Sent Events in a format byte-identical to OpenAI's
// streaming responses (`data: {json}\n\n` ... `data: [DONE]\n\n`). Every
// language's OpenAI stream parser consumes this unchanged — that's what makes
// "any language" true for streaming.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// newSSEWriter sets streaming headers and returns a writer, or false if the
// ResponseWriter cannot flush (no streaming possible).
func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, true
}

// chunk serializes and writes one chat completion chunk.
func (s *sseWriter) chunk(c *openai.ChatCompletionChunk) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.raw(data)
}

// raw writes a pre-encoded JSON payload as one SSE data event.
func (s *sseWriter) raw(data []byte) error {
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// done writes the terminal [DONE] sentinel.
func (s *sseWriter) done() {
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.flusher.Flush()
}

// errorEvent writes an OpenAI-shaped error as an SSE data event. Used when a
// failure occurs after the stream has already started (headers sent).
func (s *sseWriter) errorEvent(e *openai.ErrorResponse) {
	if data, err := json.Marshal(e); err == nil {
		s.raw(data)
	}
}

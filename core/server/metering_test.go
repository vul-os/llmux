package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/pricing"
)

// newMeteredServer wires a server whose upstream is `up`, a wildcard route to a
// passthrough provider, and a capture logger so tests can assert usage records.
func newMeteredServer(t *testing.T, up *httptest.Server) (*Server, *captureLogger) {
	t.Helper()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "k"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	cl := &captureLogger{}
	s.usage = cl
	return s, cl
}

func doPost(s *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

// --- 1. Embeddings metering --------------------------------------------------

func TestEmbeddingsMetered(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(openai.EmbeddingResponse{
			Object: "list",
			Data:   []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: []float64{0.1}}},
			Model:  "text-embedding-3-small",
			Usage:  &openai.Usage{PromptTokens: 1_000_000, TotalTokens: 1_000_000},
		})
	}))
	defer up.Close()

	s, cl := newMeteredServer(t, up)
	// Price the embedding model so a non-zero cost is metered.
	priceModel(t, s, "openai/text-embedding-3-small")

	rec := doPost(s, "/v1/embeddings", `{"model":"openai/text-embedding-3-small","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 usage record, got %d", len(cl.recs))
	}
	r := cl.recs[0]
	if r.Total != 1_000_000 {
		t.Fatalf("embedding tokens not metered: %+v", r)
	}
	if r.CostUSD <= 0 {
		t.Fatalf("embedding cost not metered: %+v", r)
	}
}

// --- 2. Forward-route metering -----------------------------------------------

func TestForwardCompletionsMetered(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"cmpl-1","object":"text_completion","model":"gpt-3.5-turbo-instruct","choices":[{"text":"hi"}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000}}`))
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)
	priceModel(t, s, "openai/gpt-3.5-turbo-instruct")

	rec := doPost(s, "/v1/completions", `{"model":"openai/gpt-3.5-turbo-instruct","prompt":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertOneMeteredRecord(t, cl, 2_000_000, true)
}

func TestForwardResponsesMetered(t *testing.T) {
	// /v1/responses reports input_tokens/output_tokens (not prompt/completion).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-1","object":"response","model":"gpt-4o","usage":{"input_tokens":1000000,"output_tokens":1000000,"total_tokens":2000000}}`))
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)
	priceModel(t, s, "openai/gpt-4o")

	rec := doPost(s, "/v1/responses", `{"model":"openai/gpt-4o","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertOneMeteredRecord(t, cl, 2_000_000, true)
}

func TestForwardImagesAudited(t *testing.T) {
	// Images carry no usage object; we still emit an auditable record (tokens=0).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"created":1,"data":[{"url":"http://x/y.png"}]}`))
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)

	rec := doPost(s, "/v1/images/generations", `{"model":"openai/dall-e-3","prompt":"a cat"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 auditable record for image gen, got %d", len(cl.recs))
	}
	if cl.recs[0].Model != "openai/dall-e-3" {
		t.Fatalf("image record model=%q", cl.recs[0].Model)
	}
	if cl.recs[0].Total != 0 {
		t.Fatalf("image record should be tokens=0 (no usage), got %+v", cl.recs[0])
	}
}

func TestForwardErrorNotMetered(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)

	rec := doPost(s, "/v1/completions", `{"model":"openai/x","prompt":"hi"}`)
	if rec.Code != 400 {
		t.Fatalf("status=%d", rec.Code)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 0 {
		t.Fatalf("upstream error must not be metered, got %d records", len(cl.recs))
	}
}

// --- 3. Streaming chat without include_usage now meters ----------------------

// TestStreamMetersViaInjectedUsage: the upstream honors the server-injected
// include_usage and returns a final usage chunk even though the CLIENT did not
// ask for it. The stream must be metered from that chunk.
func TestStreamMetersViaInjectedUsage(t *testing.T) {
	var sawIncludeUsage bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if so, ok := body["stream_options"].(map[string]any); ok {
			sawIncludeUsage, _ = so["include_usage"].(bool)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		fl.Flush()
		w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":1000000,\"completion_tokens\":1000000,\"total_tokens\":2000000}}\n\n"))
		fl.Flush()
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)
	priceModel(t, s, "openai/gpt-4o")

	// NOTE: client did NOT set stream_options.include_usage.
	rec := doPost(s, "/v1/chat/completions", `{"model":"openai/gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !sawIncludeUsage {
		t.Fatal("server must inject stream_options.include_usage=true upstream")
	}
	assertOneMeteredRecord(t, cl, 2_000_000, true)
}

// TestStreamMetersViaEstimate: upstream omits usage entirely. The stream must
// STILL be metered via the token estimate from streamed content.
func TestStreamMetersViaEstimate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// ~40 chars of content -> ~10 completion tokens estimated.
		w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"the quick brown fox jumps over lazy dogs!\"}}]}\n\n"))
		fl.Flush()
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)
	priceModel(t, s, "openai/gpt-4o")

	rec := doPost(s, "/v1/chat/completions", `{"model":"openai/gpt-4o","stream":true,"messages":[{"role":"user","content":"hi there"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 metered record from estimate, got %d", len(cl.recs))
	}
	r := cl.recs[0]
	if r.Total <= 0 || r.CostUSD <= 0 {
		t.Fatalf("estimate did not meter non-zero usage/cost: %+v", r)
	}
	if !r.Stream {
		t.Fatalf("record should be marked streaming: %+v", r)
	}
}

// TestForwardLargeStreamMeteredByEstimate: a streamed forward whose final usage
// chunk lands BEYOND the bounded meter tap (maxMeterTapBytes). Previously this
// silently billed cost_usd:0; now it must be metered via the served-bytes
// estimate so large forwards aren't free.
func TestForwardLargeStreamMeteredByEstimate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// Emit well over maxMeterTapBytes (1 MiB) of content so the usage chunk
		// at the end is past the tap window.
		chunk := []byte("data: {\"id\":\"c\",\"object\":\"text_completion\",\"choices\":[{\"text\":\"" + strings.Repeat("x", 4000) + "\"}]}\n\n")
		written := 0
		for written < (maxMeterTapBytes + 256*1024) {
			w.Write(chunk)
			written += len(chunk)
			fl.Flush()
		}
		// Final usage chunk — beyond the tap, so it won't be parsed.
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)
	priceModel(t, s, "openai/gpt-3.5-turbo-instruct")

	rec := doPost(s, "/v1/completions", `{"model":"openai/gpt-3.5-turbo-instruct","stream":true,"prompt":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 metered record for large forward stream, got %d", len(cl.recs))
	}
	r := cl.recs[0]
	if r.Total <= 0 || r.CostUSD <= 0 {
		t.Fatalf("large forward stream not metered (silent cost=0 regression): %+v", r)
	}
	if !r.Stream {
		t.Fatalf("record should be marked streaming: %+v", r)
	}
}

// TestStreamMetersOnClientDisconnect: tokens are served, then the client side
// fails mid-stream (write error after the first chunk). The request must STILL
// be metered for what was served so far — not billed to nobody.
func TestStreamMetersOnClientDisconnect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// Several content chunks, no usage chunk; the client will drop after one.
		for i := 0; i < 5; i++ {
			w.Write([]byte("data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"the quick brown fox \"}}]}\n\n"))
			fl.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	s, cl := newMeteredServer(t, up)
	priceModel(t, s, "openai/gpt-4o")

	// failingWriter accepts the first SSE write, then errors — modeling a client
	// disconnect / broken pipe mid-stream.
	fw := &failingWriter{hdr: http.Header{}, failAfter: 1}
	body := `{"model":"openai/gpt-4o","stream":true,"messages":[{"role":"user","content":"hi there"}]}`
	s.Handler().ServeHTTP(fw, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 metered record after mid-stream disconnect, got %d", len(cl.recs))
	}
	r := cl.recs[0]
	if r.Total <= 0 || r.CostUSD <= 0 {
		t.Fatalf("served tokens not metered on disconnect: %+v", r)
	}
	if !r.Stream {
		t.Fatalf("record should be marked streaming: %+v", r)
	}
}

// failingWriter is an http.ResponseWriter+Flusher whose Write starts failing
// after failAfter successful writes, modeling a client disconnect mid-stream.
type failingWriter struct {
	hdr       http.Header
	writes    int
	failAfter int
	status    int
}

func (f *failingWriter) Header() http.Header { return f.hdr }
func (f *failingWriter) WriteHeader(s int)   { f.status = s }
func (f *failingWriter) Flush()              {}
func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	if f.writes > f.failAfter {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// --- helpers ----------------------------------------------------------------

// priceModel injects a unit price for a model so metering yields a non-zero cost.
func priceModel(t *testing.T, s *Server, model string) {
	t.Helper()
	s.catalog.SetSource("test-"+model, 0, map[string]pricing.Price{
		model: {Model: model, Provider: "openai", InputPerMTok: 1, OutputPerMTok: 1},
	})
}

func assertOneMeteredRecord(t *testing.T, cl *captureLogger, wantTotal int, wantCost bool) {
	t.Helper()
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 usage record, got %d", len(cl.recs))
	}
	r := cl.recs[0]
	if r.Total != wantTotal {
		t.Fatalf("metered total=%d, want %d (%+v)", r.Total, wantTotal, r)
	}
	if wantCost && r.CostUSD <= 0 {
		t.Fatalf("expected non-zero cost, got %+v", r)
	}
}

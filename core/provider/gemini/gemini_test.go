package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

func newTestProvider(url string) *Provider {
	return New(config.ProviderConfig{Name: "gemini", Type: config.TypeGemini, BaseURL: url, APIKey: "k"})
}

func TestGeminiUnaryAndRequestTranslation(t *testing.T) {
	var captured generateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("missing api key header")
		}
		if !strings.Contains(r.URL.Path, ":generateContent") {
			t.Errorf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		resp := generateResponse{
			Candidates: []candidate{{
				Content:      content{Role: "model", Parts: []part{{Text: "Hi there"}}},
				FinishReason: "STOP",
			}},
			UsageMetadata: &usageMetadata{PromptTokenCount: 4, CandidatesTokenCount: 2, TotalTokenCount: 6},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{
		Model: "gemini",
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("be nice")},
			{Role: "user", Content: openai.Str("hello")},
		},
	}
	resp, err := p.ChatCompletion(context.Background(), req, "gemini-1.5-pro", nil)
	if err != nil {
		t.Fatal(err)
	}
	if captured.SystemInstruction == nil || captured.SystemInstruction.Parts[0].Text != "be nice" {
		t.Errorf("system instruction=%+v", captured.SystemInstruction)
	}
	if len(captured.Contents) != 1 || captured.Contents[0].Role != "user" {
		t.Errorf("contents=%+v", captured.Contents)
	}
	if resp.Choices[0].Message.Content.String() != "Hi there" {
		t.Errorf("content=%q", resp.Choices[0].Message.Content.String())
	}
	if resp.Usage.TotalTokens != 6 {
		t.Errorf("usage=%+v", resp.Usage)
	}
}

func TestGeminiToolCallResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := generateResponse{
			Candidates: []candidate{{
				Content: content{Role: "model", Parts: []part{
					{FunctionCall: &fnCall{Name: "lookup", Args: json.RawMessage(`{"id":1}`)}},
				}},
				FinishReason: "STOP",
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{Model: "g", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
	resp, err := p.ChatCompletion(context.Background(), req, "g", nil)
	if err != nil {
		t.Fatal(err)
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "lookup" || tc[0].Function.Arguments != `{"id":1}` {
		t.Fatalf("tool_calls=%+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish=%q", resp.Choices[0].FinishReason)
	}
}

func TestGeminiStream(t *testing.T) {
	chunks := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") != "sse" {
			t.Errorf("missing alt=sse")
		}
		fl := w.(http.Flusher)
		for _, c := range chunks {
			w.Write([]byte("data: " + c + "\n\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{
		Model: "g", Stream: true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
		Messages:      []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	}
	var content, role, finish string
	var usageTotal int
	err := p.ChatCompletionStream(context.Background(), req, "g", nil, func(c *openai.ChatCompletionChunk) error {
		if len(c.Choices) > 0 {
			if c.Choices[0].Delta.Role != "" {
				role = c.Choices[0].Delta.Role
			}
			content += c.Choices[0].Delta.Content
			if c.Choices[0].FinishReason != nil {
				finish = *c.Choices[0].FinishReason
			}
		}
		if c.Usage != nil {
			usageTotal = c.Usage.TotalTokens
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if role != "assistant" || content != "Hello" || finish != "stop" {
		t.Fatalf("role=%q content=%q finish=%q", role, content, finish)
	}
	if usageTotal != 4 {
		t.Errorf("usage=%d", usageTotal)
	}
}

func TestGeminiEmbeddingsSingle(t *testing.T) {
	var capturedPath string
	var captured embedContentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("missing api key header")
		}
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		json.NewEncoder(w).Encode(embedContentResponse{Embedding: embedding{Values: []float64{0.1, 0.2, 0.3}}})
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	in, _ := json.Marshal("hello world")
	req := &openai.EmbeddingRequest{Model: "gemini-embed", Input: in}
	resp, err := p.Embeddings(context.Background(), req, "text-embedding-004", nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(capturedPath, ":embedContent") {
		t.Errorf("path=%s", capturedPath)
	}
	if len(captured.Content.Parts) != 1 || captured.Content.Parts[0].Text != "hello world" {
		t.Errorf("content=%+v", captured.Content)
	}
	if resp.Object != "list" || resp.Model != "gemini-embed" {
		t.Errorf("object=%q model=%q", resp.Object, resp.Model)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data len=%d", len(resp.Data))
	}
	if resp.Data[0].Index != 0 || resp.Data[0].Object != "embedding" {
		t.Errorf("data[0]=%+v", resp.Data[0])
	}
	if got := resp.Data[0].Embedding; len(got) != 3 || got[0] != 0.1 || got[2] != 0.3 {
		t.Errorf("embedding=%v", got)
	}
}

func TestGeminiEmbeddingsBatch(t *testing.T) {
	var capturedPath string
	var captured batchEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		json.NewEncoder(w).Encode(batchEmbedResponse{Embeddings: []embedding{
			{Values: []float64{1, 2}},
			{Values: []float64{3, 4}},
			{Values: []float64{5, 6}},
		}})
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	in, _ := json.Marshal([]string{"a", "b", "c"})
	req := &openai.EmbeddingRequest{Model: "gemini-embed", Input: in}
	resp, err := p.Embeddings(context.Background(), req, "text-embedding-004", nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(capturedPath, ":batchEmbedContents") {
		t.Errorf("path=%s", capturedPath)
	}
	if len(captured.Requests) != 3 {
		t.Fatalf("requests len=%d", len(captured.Requests))
	}
	for i, item := range captured.Requests {
		if item.Model != "models/text-embedding-004" {
			t.Errorf("requests[%d].Model=%q", i, item.Model)
		}
		if len(item.Content.Parts) != 1 {
			t.Errorf("requests[%d].Content=%+v", i, item.Content)
		}
	}
	if captured.Requests[1].Content.Parts[0].Text != "b" {
		t.Errorf("requests[1] text=%q", captured.Requests[1].Content.Parts[0].Text)
	}

	if len(resp.Data) != 3 {
		t.Fatalf("data len=%d", len(resp.Data))
	}
	for i, d := range resp.Data {
		if d.Index != i {
			t.Errorf("data[%d].Index=%d", i, d.Index)
		}
	}
	if got := resp.Data[2].Embedding; len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Errorf("embedding[2]=%v", got)
	}
}

func TestGeminiEmbeddingsEmptyInput(t *testing.T) {
	p := newTestProvider("http://unused")
	_, err := p.Embeddings(context.Background(), &openai.EmbeddingRequest{}, "embed", nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestGeminiErrorFromResponseStatusToType(t *testing.T) {
	tests := []struct {
		status   int
		wantType string
	}{
		{400, "invalid_request_error"},
		{401, "authentication_error"},
		{403, "permission_error"},
		{404, "not_found_error"},
		{429, "rate_limit_error"},
		{500, "api_error"},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
			// Gemini puts RESOURCE_EXHAUSTED / INVALID_ARGUMENT in status; must NOT leak.
			w.Write([]byte(`{"error":{"code":` + itoa(tt.status) + `,"message":"quota or arg problem","status":"RESOURCE_EXHAUSTED"}}`))
		}))
		p := newTestProvider(srv.URL)
		req := &openai.ChatCompletionRequest{Model: "c", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
		_, err := p.ChatCompletion(context.Background(), req, "c", nil)
		srv.Close()
		pe, ok := err.(*provider.Error)
		if !ok {
			t.Fatalf("status %d: err type %T", tt.status, err)
		}
		if pe.Body.Error.Type != tt.wantType {
			t.Errorf("status %d: type=%q want %q", tt.status, pe.Body.Error.Type, tt.wantType)
		}
		if pe.Body.Error.Type == "RESOURCE_EXHAUSTED" {
			t.Errorf("status %d: raw gemini status leaked into type", tt.status)
		}
		if pe.Body.Error.Message != "quota or arg problem" {
			t.Errorf("status %d: message=%q want preserved", tt.status, pe.Body.Error.Message)
		}
	}
}

func TestGeminiErrorContextLengthExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"code":400,"message":"The input token count exceeds the maximum allowed","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{Model: "c", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
	_, err := p.ChatCompletion(context.Background(), req, "c", nil)
	pe := err.(*provider.Error)
	if pe.Body.Error.Code != "context_length_exceeded" {
		t.Errorf("code=%q want context_length_exceeded", pe.Body.Error.Code)
	}
}

func TestGeminiErrorCapturesRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining-requests", "5")
		w.Header().Set("retry-after", "12")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"code":429,"message":"rate limited","status":"RESOURCE_EXHAUSTED"}}`))
	}))
	defer srv.Close()
	p := newTestProvider(srv.URL)
	ctx, sink := provider.WithHeaderSink(context.Background())
	req := &openai.ChatCompletionRequest{Model: "c", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
	_, _ = p.ChatCompletion(ctx, req, "c", nil)
	h := sink.Header()
	if h.Get("retry-after") != "12" {
		t.Errorf("retry-after not captured: %v", h)
	}
	if h.Get("x-ratelimit-remaining-requests") != "5" {
		t.Errorf("x-ratelimit header not captured: %v", h)
	}
}

// itoa avoids importing strconv just for the table test bodies.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestGeminiStreamEmptyToolArgs(t *testing.T) {
	events := []string{
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping"}}]},"finishReason":"STOP"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, e := range events {
			w.Write([]byte("data: " + e + "\n\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()
	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{Model: "c", Stream: true, Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
	var args, name string
	p.ChatCompletionStream(context.Background(), req, "c", nil, func(c *openai.ChatCompletionChunk) error {
		if len(c.Choices) == 0 {
			return nil
		}
		for _, tc := range c.Choices[0].Delta.ToolCalls {
			if tc.Function.Name != "" {
				name = tc.Function.Name
			}
			args += tc.Function.Arguments
		}
		return nil
	})
	if name != "ping" {
		t.Errorf("name=%q", name)
	}
	if args != "{}" {
		t.Errorf("empty tool args streamed as %q want {}", args)
	}
}

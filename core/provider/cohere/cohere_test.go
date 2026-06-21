package cohere

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
	return New(config.ProviderConfig{Name: "cohere", BaseURL: url, APIKey: "k"})
}

func TestCohereUnaryAndRequestTranslation(t *testing.T) {
	var captured chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing/incorrect auth header: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		resp := chatResponse{
			ID:           "c_1",
			FinishReason: "COMPLETE",
			Message: responseMessage{
				Role:    "assistant",
				Content: []textContent{{Type: "text", Text: "Sure."}},
				ToolCalls: []toolCall{{
					ID: "tc_1", Type: "function",
					Function: functionCall{Name: "get_weather", Arguments: `{"city":"NYC"}`},
				}},
			},
			Usage: usage{Tokens: tokens{InputTokens: 10, OutputTokens: 5}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	temp := 0.7
	topP := 0.9
	maxTok := 256
	req := &openai.ChatCompletionRequest{
		Model: "command", Temperature: &temp, TopP: &topP, MaxTokens: &maxTok,
		Stop: func() *openai.StringOrArray {
			var s openai.StringOrArray
			json.Unmarshal([]byte(`["STOP"]`), &s)
			return &s
		}(),
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("be brief")},
			{Role: "user", Content: openai.Str("weather?")},
		},
		Tools: []openai.Tool{{Type: "function", Function: openai.FunctionDef{
			Name: "get_weather", Description: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	resp, err := p.ChatCompletion(context.Background(), req, "command-r-plus", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Request translation assertions.
	if captured.Model != "command-r-plus" {
		t.Errorf("model=%q", captured.Model)
	}
	if captured.P == nil || *captured.P != 0.9 {
		t.Errorf("p=%v want 0.9", captured.P)
	}
	if captured.MaxTokens == nil || *captured.MaxTokens != 256 {
		t.Errorf("max_tokens=%v", captured.MaxTokens)
	}
	if len(captured.StopSequences) != 1 || captured.StopSequences[0] != "STOP" {
		t.Errorf("stop_sequences=%v", captured.StopSequences)
	}
	if len(captured.Messages) != 2 {
		t.Fatalf("messages=%+v", captured.Messages)
	}
	if captured.Messages[0].Role != "system" || string(captured.Messages[0].Content) != `"be brief"` {
		t.Errorf("system msg=%+v", captured.Messages[0])
	}
	if captured.Messages[1].Role != "user" || string(captured.Messages[1].Content) != `"weather?"` {
		t.Errorf("user msg=%+v", captured.Messages[1])
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Type != "function" ||
		captured.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools=%+v", captured.Tools)
	}

	// Response translation assertions.
	if resp.Choices[0].Message.Content.String() != "Sure." {
		t.Errorf("content=%q", resp.Choices[0].Message.Content.String())
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "tc_1" || tc[0].Type != "function" ||
		tc[0].Function.Name != "get_weather" || tc[0].Function.Arguments != `{"city":"NYC"}` {
		t.Errorf("tool_calls=%+v", tc)
	}
	// tool calls present overrides COMPLETE -> tool_calls.
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish=%q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 15 {
		t.Errorf("usage=%+v", resp.Usage)
	}
	if resp.Model != "command" {
		t.Errorf("model=%q want requested model", resp.Model)
	}
}

func TestCohereUnaryFinishReasonMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			ID:           "c_2",
			FinishReason: "MAX_TOKENS",
			Message: responseMessage{
				Role:    "assistant",
				Content: []textContent{{Type: "text", Text: "truncated"}},
			},
			Usage: usage{Tokens: tokens{InputTokens: 3, OutputTokens: 7}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{
		Model:    "command",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	}
	resp, err := p.ChatCompletion(context.Background(), req, "command", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].FinishReason != "length" {
		t.Errorf("finish=%q want length", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 10 {
		t.Errorf("usage total=%d want 10", resp.Usage.TotalTokens)
	}
}

func TestCohereStream(t *testing.T) {
	events := []string{
		`{"type":"message-start","delta":{"message":{"role":"assistant"}}}`,
		`{"type":"content-start","index":0,"delta":{"message":{"content":{"type":"text","text":""}}}}`,
		`{"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"Hel"}}}}`,
		`{"type":"content-delta","index":0,"delta":{"message":{"content":{"text":"lo"}}}}`,
		`{"type":"content-end","index":0}`,
		`{"type":"message-end","delta":{"finish_reason":"COMPLETE","usage":{"tokens":{"input_tokens":8,"output_tokens":3}}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, e := range events {
			w.Write([]byte("event: x\ndata: " + e + "\n\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{
		Model: "command", Stream: true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
		Messages:      []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	}

	var content, role, finish string
	var usageTotal int
	err := p.ChatCompletionStream(context.Background(), req, "command", nil, func(c *openai.ChatCompletionChunk) error {
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
	if role != "assistant" {
		t.Errorf("role=%q", role)
	}
	if content != "Hello" {
		t.Errorf("content=%q", content)
	}
	if finish != "stop" {
		t.Errorf("finish=%q", finish)
	}
	if usageTotal != 11 {
		t.Errorf("usage total=%d want 11", usageTotal)
	}
}

func TestCohereStreamToolCalls(t *testing.T) {
	events := []string{
		`{"type":"message-start","delta":{"message":{"role":"assistant"}}}`,
		`{"type":"tool-call-start","index":0,"delta":{"message":{"tool_calls":{"id":"tc_9","type":"function","function":{"name":"search"}}}}}`,
		`{"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":{"function":{"arguments":"{\"q\":"}}}}}`,
		`{"type":"tool-call-delta","index":0,"delta":{"message":{"tool_calls":{"function":{"arguments":"\"go\"}"}}}}}`,
		`{"type":"tool-call-end","index":0}`,
		`{"type":"message-end","delta":{"finish_reason":"TOOL_CALL"}}`,
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
	req := &openai.ChatCompletionRequest{Model: "command", Stream: true,
		Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}

	var name, args, finish string
	var toolIndex = -1
	err := p.ChatCompletionStream(context.Background(), req, "command", nil, func(c *openai.ChatCompletionChunk) error {
		if len(c.Choices) == 0 {
			return nil
		}
		for _, tc := range c.Choices[0].Delta.ToolCalls {
			if tc.Function.Name != "" {
				name = tc.Function.Name
			}
			if tc.Index != nil {
				toolIndex = *tc.Index
			}
			args += tc.Function.Arguments
		}
		if c.Choices[0].FinishReason != nil {
			finish = *c.Choices[0].FinishReason
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if name != "search" {
		t.Errorf("name=%q", name)
	}
	if args != `{"q":"go"}` {
		t.Errorf("args=%q", args)
	}
	if toolIndex != 0 {
		t.Errorf("tool index=%d want 0", toolIndex)
	}
	if finish != "tool_calls" {
		t.Errorf("finish=%q", finish)
	}
}

func TestCohereAssistantToolCallRequestTranslation(t *testing.T) {
	var captured chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		json.NewEncoder(w).Encode(chatResponse{ID: "c", FinishReason: "COMPLETE",
			Message: responseMessage{Role: "assistant", Content: []textContent{{Type: "text", Text: "ok"}}}})
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	req := &openai.ChatCompletionRequest{
		Model: "command",
		Messages: []openai.Message{
			{Role: "user", Content: openai.Str("weather?")},
			{Role: "assistant", ToolCalls: []openai.ToolCall{{
				ID: "tc_1", Type: "function",
				Function: openai.FunctionCall{Name: "get_weather", Arguments: `{"city":"NYC"}`},
			}}},
			{Role: "tool", ToolCallID: "tc_1", Content: openai.Str("sunny")},
		},
	}
	if _, err := p.ChatCompletion(context.Background(), req, "command", nil); err != nil {
		t.Fatal(err)
	}
	if len(captured.Messages) != 3 {
		t.Fatalf("messages=%+v", captured.Messages)
	}
	asst := captured.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 ||
		asst.ToolCalls[0].ID != "tc_1" || asst.ToolCalls[0].Function.Name != "get_weather" ||
		asst.ToolCalls[0].Function.Arguments != `{"city":"NYC"}` {
		t.Errorf("assistant msg=%+v", asst)
	}
	tool := captured.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "tc_1" || string(tool.Content) != `"sunny"` {
		t.Errorf("tool msg=%+v", tool)
	}
}

func TestCohereEmbeddings(t *testing.T) {
	var captured embedRequest
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing/incorrect auth header: %q", r.Header.Get("Authorization"))
		}
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		resp := embedResponse{Embeddings: embeddingsByType{Float: [][]float64{
			{0.1, 0.2, 0.3},
			{0.4, 0.5, 0.6},
		}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	in, _ := json.Marshal([]string{"hello", "world"})
	req := &openai.EmbeddingRequest{Model: "cohere-model", Input: in}
	resp, err := p.Embeddings(context.Background(), req, "embed-english-v3.0", nil)
	if err != nil {
		t.Fatal(err)
	}

	if capturedPath != "/embed" {
		t.Errorf("path=%q", capturedPath)
	}
	if captured.Model != "embed-english-v3.0" {
		t.Errorf("model=%q", captured.Model)
	}
	if captured.InputType != "search_document" {
		t.Errorf("input_type=%q", captured.InputType)
	}
	if len(captured.EmbeddingTypes) != 1 || captured.EmbeddingTypes[0] != "float" {
		t.Errorf("embedding_types=%v", captured.EmbeddingTypes)
	}
	if len(captured.Texts) != 2 || captured.Texts[0] != "hello" || captured.Texts[1] != "world" {
		t.Errorf("texts=%v", captured.Texts)
	}

	if resp.Object != "list" || resp.Model != "cohere-model" {
		t.Errorf("object=%q model=%q", resp.Object, resp.Model)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data len=%d", len(resp.Data))
	}
	for i, d := range resp.Data {
		if d.Index != i {
			t.Errorf("data[%d].Index=%d", i, d.Index)
		}
		if d.Object != "embedding" {
			t.Errorf("data[%d].Object=%q", i, d.Object)
		}
	}
	if got := resp.Data[0].Embedding; len(got) != 3 || got[0] != 0.1 {
		t.Errorf("embedding[0]=%v", got)
	}
	if got := resp.Data[1].Embedding; len(got) != 3 || got[2] != 0.6 {
		t.Errorf("embedding[1]=%v", got)
	}
}

func TestCohereEmbeddingsSingleString(t *testing.T) {
	var captured embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		json.NewEncoder(w).Encode(embedResponse{Embeddings: embeddingsByType{Float: [][]float64{{1, 2}}}})
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	in, _ := json.Marshal("just one")
	resp, err := p.Embeddings(context.Background(), &openai.EmbeddingRequest{Model: "m", Input: in}, "embed", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Texts) != 1 || captured.Texts[0] != "just one" {
		t.Errorf("texts=%v", captured.Texts)
	}
	if len(resp.Data) != 1 || resp.Data[0].Index != 0 {
		t.Errorf("data=%+v", resp.Data)
	}
}

func TestCohereEmbeddingsEmptyInput(t *testing.T) {
	p := newTestProvider("http://unused")
	_, err := p.Embeddings(context.Background(), &openai.EmbeddingRequest{}, "embed", nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

// helper: build an *http.Response with the given status, body, and headers.
func makeResp(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
	}
}

func TestCohereErrorTypeNormalization(t *testing.T) {
	p := newTestProvider("http://unused")
	cases := []struct {
		status   int
		wantType string
	}{
		{400, "invalid_request_error"},
		{401, "authentication_error"},
		{403, "permission_error"},
		{404, "not_found_error"},
		{422, "invalid_request_error"},
		{429, "rate_limit_error"},
		{500, "api_error"},
		{503, "api_error"},
	}
	for _, tc := range cases {
		resp := makeResp(tc.status, `{"message":"boom"}`, nil)
		e := p.errorFromResponse(context.Background(), resp)
		if e.Body.Error.Type == "upstream_error" {
			t.Errorf("status %d: type must not be hardcoded upstream_error", tc.status)
		}
		if e.Body.Error.Type != tc.wantType {
			t.Errorf("status %d: type=%q want %q", tc.status, e.Body.Error.Type, tc.wantType)
		}
		if e.Body.Error.Message != "boom" {
			t.Errorf("status %d: message=%q want upstream message preserved", tc.status, e.Body.Error.Message)
		}
	}
}

func TestCohereContextLengthCode(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		want   string
	}{
		{400, "the request exceeds the maximum context length", "context_length_exceeded"},
		{413, "too many tokens for the context window", "context_length_exceeded"},
		{400, "your input has too many tokens, it is too long", "context_length_exceeded"},
		{400, "you are out of credits", ""}, // no context/token keyword
		{400, "token limit exceeded", ""},   // "token" but no length/window/maximum/too long keyword
		{400, "invalid api parameter", ""},
		{429, "context length maximum exceeded", ""}, // wrong status
		{500, "context window maximum", ""},          // wrong status
	}
	for _, tc := range cases {
		if got := contextLengthCode(tc.status, tc.msg); got != tc.want {
			t.Errorf("contextLengthCode(%d,%q)=%q want %q", tc.status, tc.msg, got, tc.want)
		}
	}
}

func TestCohereErrorContextLengthCodeWired(t *testing.T) {
	p := newTestProvider("http://unused")
	resp := makeResp(400, `{"message":"prompt exceeds the maximum context length"}`, nil)
	e := p.errorFromResponse(context.Background(), resp)
	if e.Body.Error.Code != "context_length_exceeded" {
		t.Errorf("code=%q want context_length_exceeded", e.Body.Error.Code)
	}
}

func TestCohereErrorCapturesRateLimitHeaders(t *testing.T) {
	p := newTestProvider("http://unused")
	ctx, sink := provider.WithHeaderSink(context.Background())
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("Retry-After", "30")
	resp := makeResp(429, `{"message":"slow down"}`, h)
	_ = p.errorFromResponse(ctx, resp)
	if got := sink.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("x-ratelimit-remaining=%q want 0", got)
	}
	if got := sink.Header().Get("Retry-After"); got != "30" {
		t.Errorf("retry-after=%q want 30", got)
	}
}

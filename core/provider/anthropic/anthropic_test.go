package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

func newTestProvider(url string) *Provider {
	return New(config.ProviderConfig{Name: "anthropic", Type: config.TypeAnthropic, BaseURL: url, APIKey: "k"})
}

func TestAnthropicUnaryAndRequestTranslation(t *testing.T) {
	var captured messagesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth/version headers")
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		resp := messagesResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "claude-x",
			Content: []block{
				{Type: "text", Text: "Sure."},
				{Type: "tool_use", ID: "tu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"NYC"}`)},
			},
			StopReason: "tool_use",
			Usage:      usage{InputTokens: 10, OutputTokens: 5},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := newTestProvider(srv.URL)
	temp := 0.7
	req := &openai.ChatCompletionRequest{
		Model: "claude", Temperature: &temp,
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("be brief")},
			{Role: "user", Content: openai.Str("weather?")},
		},
		Tools: []openai.Tool{{Type: "function", Function: openai.FunctionDef{
			Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	resp, err := p.ChatCompletion(context.Background(), req, "claude-3-5-sonnet", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Request translation assertions.
	if captured.System != "be brief" {
		t.Errorf("system=%q", captured.System)
	}
	if captured.Model != "claude-3-5-sonnet" {
		t.Errorf("model=%q", captured.Model)
	}
	if captured.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens=%d want default", captured.MaxTokens)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Name != "get_weather" {
		t.Errorf("tools=%+v", captured.Tools)
	}

	// Response translation assertions.
	if resp.Choices[0].Message.Content.String() != "Sure." {
		t.Errorf("content=%q", resp.Choices[0].Message.Content.String())
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "get_weather" || tc[0].Function.Arguments != `{"city":"NYC"}` {
		t.Errorf("tool_calls=%+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish=%q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("usage=%+v", resp.Usage)
	}
}

func TestAnthropicStream(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_2","model":"claude-x","usage":{"input_tokens":8,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
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
		Model: "claude", Stream: true,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
		Messages:      []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	}

	var content string
	var role string
	var finish string
	var usageTotal int
	err := p.ChatCompletionStream(context.Background(), req, "claude-3", nil, func(c *openai.ChatCompletionChunk) error {
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

func TestAnthropicStreamToolCalls(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"m","model":"c","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_9","name":"search"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"go\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
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

	var name, args, finish string
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
		if c.Choices[0].FinishReason != nil {
			finish = *c.Choices[0].FinishReason
		}
		return nil
	})
	if name != "search" {
		t.Errorf("name=%q", name)
	}
	if args != `{"q":"go"}` {
		t.Errorf("args=%q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish=%q", finish)
	}
}

func TestAnthropicErrorFromResponseStatusToType(t *testing.T) {
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
		{529, "api_error"},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"upstream said boom"}}`))
		}))
		p := newTestProvider(srv.URL)
		req := &openai.ChatCompletionRequest{Model: "c", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
		_, err := p.ChatCompletion(context.Background(), req, "c", nil)
		srv.Close()
		pe, ok := err.(*provider.Error)
		if !ok {
			t.Fatalf("status %d: err type %T", tt.status, err)
		}
		if pe.StatusCode != tt.status {
			t.Errorf("status %d: StatusCode=%d", tt.status, pe.StatusCode)
		}
		if pe.Body.Error.Type != tt.wantType {
			t.Errorf("status %d: type=%q want %q (raw provider type must be replaced)", tt.status, pe.Body.Error.Type, tt.wantType)
		}
		if pe.Body.Error.Message != "upstream said boom" {
			t.Errorf("status %d: message=%q want upstream preserved", tt.status, pe.Body.Error.Message)
		}
	}
}

func TestAnthropicErrorContextLengthExceeded(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		message  string
		wantCode string
	}{
		{"400 token maximum", 400, "prompt is too long: 250000 tokens exceeds the maximum", "context_length_exceeded"},
		{"413 context window", 413, "input exceeds the context window length", "context_length_exceeded"},
		{"400 unrelated", 400, "invalid field 'foo'", ""},
		{"429 token (wrong status)", 429, "token length exceeded", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"` + c.message + `"}}`))
			}))
			defer srv.Close()
			p := newTestProvider(srv.URL)
			req := &openai.ChatCompletionRequest{Model: "c", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
			_, err := p.ChatCompletion(context.Background(), req, "c", nil)
			pe := err.(*provider.Error)
			if pe.Body.Error.Code != c.wantCode {
				t.Errorf("code=%q want %q", pe.Body.Error.Code, c.wantCode)
			}
		})
	}
}

func TestAnthropicErrorCapturesRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.Header().Set("retry-after", "30")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()
	p := newTestProvider(srv.URL)
	ctx, sink := provider.WithHeaderSink(context.Background())
	req := &openai.ChatCompletionRequest{Model: "c", Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}}}
	_, err := p.ChatCompletion(ctx, req, "c", nil)
	if _, ok := err.(*provider.Error); !ok {
		t.Fatalf("err type %T", err)
	}
	h := sink.Header()
	if h.Get("retry-after") != "30" {
		t.Errorf("retry-after not captured: %v", h)
	}
	if h.Get("x-ratelimit-remaining-requests") != "0" {
		t.Errorf("x-ratelimit header not captured: %v", h)
	}
}

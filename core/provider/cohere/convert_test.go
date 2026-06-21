package cohere

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/openai"
)

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(i int) *int           { return &i }

// --- decodeEmbeddingInput ---------------------------------------------------

func TestDecodeEmbeddingInput(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"single string", `"hello"`, []string{"hello"}, false},
		{"array", `["a","b"]`, []string{"a", "b"}, false},
		{"empty array", `[]`, []string{}, false},
		{"empty", ``, nil, true},
		{"null", `null`, nil, true},
		{"number", `5`, nil, true},
		{"object", `{}`, nil, true},
		{"array of numbers", `[1]`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeEmbeddingInput(json.RawMessage(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v want %v", got, tt.want)
				}
			}
		})
	}
}

func TestDecodeEmbeddingInputTokenArray(t *testing.T) {
	for _, in := range []string{`[1,2,3]`, `[[1,2],[3,4]]`} {
		_, err := decodeEmbeddingInput(json.RawMessage(in))
		if err == nil {
			t.Fatalf("input %s: expected error", in)
		}
		if !strings.Contains(err.Error(), "token-array embedding input is not supported by Cohere") {
			t.Fatalf("input %s: unclear error: %v", in, err)
		}
	}
	if _, err := decodeEmbeddingInput(json.RawMessage(`"hi"`)); err != nil {
		t.Fatalf("string input should work: %v", err)
	}
	if _, err := decodeEmbeddingInput(json.RawMessage(`["a","b"]`)); err != nil {
		t.Fatalf("[]string input should work: %v", err)
	}
}

// --- toCohere config & roles ------------------------------------------------

func TestToCohereScalarsAndMaxTokens(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Temperature: ptrFloat(0.6),
		TopP:        ptrFloat(0.95),
		Stream:      true,
		MaxTokens:   ptrInt(40),
		Stop:        &openai.StringOrArray{Values: []string{"S"}},
	}
	out, err := toCohere(req, "command-x")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "command-x" {
		t.Fatalf("model=%q", out.Model)
	}
	if out.Temperature == nil || *out.Temperature != 0.6 {
		t.Fatalf("temp=%v", out.Temperature)
	}
	if out.P == nil || *out.P != 0.95 {
		t.Fatalf("p=%v", out.P)
	}
	if !out.Stream {
		t.Fatal("stream not set")
	}
	if out.MaxTokens == nil || *out.MaxTokens != 40 {
		t.Fatalf("maxtokens=%v", out.MaxTokens)
	}
	if !reflect.DeepEqual(out.StopSequences, []string{"S"}) {
		t.Fatalf("stop=%v", out.StopSequences)
	}
}

func TestToCohereMaxCompletionTokens(t *testing.T) {
	out, _ := toCohere(&openai.ChatCompletionRequest{MaxCompletionTokens: ptrInt(17)}, "m")
	if out.MaxTokens == nil || *out.MaxTokens != 17 {
		t.Fatalf("maxtokens=%v", out.MaxTokens)
	}
}

func TestToCohereNoMaxTokens(t *testing.T) {
	out, _ := toCohere(&openai.ChatCompletionRequest{}, "m")
	if out.MaxTokens != nil {
		t.Fatalf("expected nil maxtokens, got %v", *out.MaxTokens)
	}
}

func TestToCohereRoleMapping(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("s")},
			{Role: "developer", Content: openai.Str("d")},
			{Role: "user", Content: openai.Str("u")},
			{Role: "tool", ToolCallID: "tc1", Content: openai.Str("r")},
		},
	}
	out, _ := toCohere(req, "m")
	if len(out.Messages) != 4 {
		t.Fatalf("messages=%+v", out.Messages)
	}
	// developer maps to system role
	if out.Messages[0].Role != "system" || out.Messages[1].Role != "system" {
		t.Fatalf("roles=%q,%q", out.Messages[0].Role, out.Messages[1].Role)
	}
	if string(out.Messages[0].Content) != `"s"` {
		t.Fatalf("sys content=%s", out.Messages[0].Content)
	}
	if out.Messages[3].Role != "tool" || out.Messages[3].ToolCallID != "tc1" {
		t.Fatalf("tool msg=%+v", out.Messages[3])
	}
	if string(out.Messages[3].Content) != `"r"` {
		t.Fatalf("tool content=%s", out.Messages[3].Content)
	}
}

func TestToCohereAssistantToolCalls(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", Content: openai.Str("thought"), ToolCalls: []openai.ToolCall{
				{ID: "tc1", Function: openai.FunctionCall{Name: "f", Arguments: `{"x":1}`}},
				{ID: "tc2", Function: openai.FunctionCall{Name: "g", Arguments: ``}},
			}},
		},
	}
	out, _ := toCohere(req, "m")
	m := out.Messages[0]
	if m.Role != "assistant" {
		t.Fatalf("role=%q", m.Role)
	}
	if string(m.Content) != `"thought"` {
		t.Fatalf("content=%s", m.Content)
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("toolcalls=%+v", m.ToolCalls)
	}
	if m.ToolCalls[0].Type != "function" || m.ToolCalls[0].Function.Name != "f" || m.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("tc0=%+v", m.ToolCalls[0])
	}
	// empty args default to {}
	if m.ToolCalls[1].Function.Arguments != `{}` {
		t.Fatalf("tc1 args=%q", m.ToolCalls[1].Function.Arguments)
	}
}

func TestToCohereAssistantNoContent(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", ToolCalls: []openai.ToolCall{
				{ID: "tc1", Function: openai.FunctionCall{Name: "f"}},
			}},
		},
	}
	out, _ := toCohere(req, "m")
	if out.Messages[0].Content != nil {
		t.Fatalf("expected nil content, got %s", out.Messages[0].Content)
	}
}

func TestToCohereTools(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Tools: []openai.Tool{
			{Type: "function", Function: openai.FunctionDef{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "other", Function: openai.FunctionDef{Name: "skip"}},
			{Function: openai.FunctionDef{Name: "g"}},
		},
	}
	out, _ := toCohere(req, "m")
	if len(out.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(out.Tools))
	}
	if out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "f" {
		t.Fatalf("tool0=%+v", out.Tools[0])
	}
	if string(out.Tools[1].Function.Parameters) != `{}` {
		t.Fatalf("g params=%s", out.Tools[1].Function.Parameters)
	}
}

// --- textPayload ------------------------------------------------------------

func TestTextPayload(t *testing.T) {
	if got := textPayload(openai.Str("")); got != nil {
		t.Fatalf("empty string -> %s", got)
	}
	if got := string(textPayload(openai.Str("hi"))); got != `"hi"` {
		t.Fatalf("string -> %s", got)
	}

	multi := openai.MessageContent{Parts: []openai.ContentPart{
		{Type: "text", Text: "a"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "https://x"}}, // non-text ignored
		{Type: "text", Text: "b"},
	}}
	got := string(textPayload(multi))
	if got != `[{"type":"text","text":"a"},{"type":"text","text":"b"}]` {
		t.Fatalf("multipart -> %s", got)
	}

	// parts with no text content -> nil
	imgOnly := openai.MessageContent{Parts: []openai.ContentPart{
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "https://x"}},
	}}
	if got := textPayload(imgOnly); got != nil {
		t.Fatalf("image-only -> %s", got)
	}
}

// --- fromCohere -------------------------------------------------------------

func TestFromCohereText(t *testing.T) {
	resp := &chatResponse{
		ID:           "id",
		FinishReason: "COMPLETE",
		Message: responseMessage{
			Content: []textContent{
				{Type: "text", Text: "Hello "},
				{Type: "", Text: "world"}, // empty type also treated as text
			},
		},
		Usage: usage{Tokens: tokens{InputTokens: 4, OutputTokens: 6}},
	}
	out := fromCohere(resp, "command-r")
	c := out.Choices[0]
	if c.Message.Content.String() != "Hello world" {
		t.Fatalf("content=%q", c.Message.Content.String())
	}
	if c.FinishReason != "stop" {
		t.Fatalf("finish=%q", c.FinishReason)
	}
	if out.Model != "command-r" {
		t.Fatalf("model=%q", out.Model)
	}
	if out.Usage.TotalTokens != 10 {
		t.Fatalf("total=%d", out.Usage.TotalTokens)
	}
}

func TestFromCohereToolCalls(t *testing.T) {
	resp := &chatResponse{
		FinishReason: "COMPLETE", // overridden to tool_calls because tool calls present
		Message: responseMessage{
			ToolCalls: []toolCall{
				{ID: "t1", Function: functionCall{Name: "f", Arguments: `{"a":1}`}},
			},
		},
	}
	out := fromCohere(resp, "m")
	c := out.Choices[0]
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "f" {
		t.Fatalf("toolcalls=%+v", c.Message.ToolCalls)
	}
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", c.FinishReason)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"COMPLETE":      "stop",
		"STOP":          "stop",
		"STOP_SEQUENCE": "stop",
		"MAX_TOKENS":    "length",
		"TOOL_CALL":     "tool_calls",
		"TOOL_CALLS":    "tool_calls",
		"":              "",
		"whatever":      "stop",
	}
	for in, want := range cases {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRawOrEmptyObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `{}`},
		{"  ", `{}`},
		{`{"a":1}`, `{"a":1}`},
		{"bad", `{}`},
	}
	for _, c := range cases {
		if got := string(rawOrEmptyObject(c.in)); got != c.want {
			t.Errorf("rawOrEmptyObject(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestArgsOrEmptyObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "{}"},
		{"   ", "{}"},
		{`{"a":1}`, `{"a":1}`},
		{"not json", "{}"},
		{`[1,2]`, `[1,2]`}, // valid json passes through
	}
	for _, c := range cases {
		if got := argsOrEmptyObject(c.in); got != c.want {
			t.Errorf("argsOrEmptyObject(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestJSONString(t *testing.T) {
	if got := string(jsonString(`a"b`)); got != `"a\"b"` {
		t.Fatalf("jsonString=%s", got)
	}
}

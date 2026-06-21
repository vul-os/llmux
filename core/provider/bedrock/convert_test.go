package bedrock

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/llmux/llmux/core/openai"
)

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(i int) *int           { return &i }

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// --- max_tokens defaulting --------------------------------------------------

func TestToInvokeMaxTokens(t *testing.T) {
	tests := []struct {
		name string
		req  *openai.ChatCompletionRequest
		want int
	}{
		{"default", &openai.ChatCompletionRequest{}, defaultMaxTokens},
		{"max_tokens", &openai.ChatCompletionRequest{MaxTokens: ptrInt(11)}, 11},
		{"max_completion_tokens", &openai.ChatCompletionRequest{MaxCompletionTokens: ptrInt(22)}, 22},
		{"max_tokens wins", &openai.ChatCompletionRequest{MaxTokens: ptrInt(3), MaxCompletionTokens: ptrInt(9)}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := toInvoke(tt.req)
			if out.MaxTokens != tt.want {
				t.Fatalf("MaxTokens=%d want %d", out.MaxTokens, tt.want)
			}
		})
	}
}

func TestToInvokeAnthropicVersion(t *testing.T) {
	out := toInvoke(&openai.ChatCompletionRequest{})
	if out.AnthropicVersion != bedrockAnthropicVersion {
		t.Fatalf("version=%q", out.AnthropicVersion)
	}
}

func TestToInvokeScalars(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Temperature: ptrFloat(0.3),
		TopP:        ptrFloat(0.8),
		Stop:        &openai.StringOrArray{Values: []string{"END"}},
	}
	out := toInvoke(req)
	if out.Temperature == nil || *out.Temperature != 0.3 {
		t.Fatalf("temp=%v", out.Temperature)
	}
	if out.TopP == nil || *out.TopP != 0.8 {
		t.Fatalf("topp=%v", out.TopP)
	}
	if !reflect.DeepEqual(out.StopSequences, []string{"END"}) {
		t.Fatalf("stop=%v", out.StopSequences)
	}
}

func TestToInvokeSystemExtraction(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("s1")},
			{Role: "developer", Content: openai.Str("d1")},
			{Role: "user", Content: openai.Str("u")},
		},
	}
	out := toInvoke(req)
	if out.System != "s1\n\nd1" {
		t.Fatalf("system=%q", out.System)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages=%+v", out.Messages)
	}
}

func TestToInvokeMergeSameRole(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "user", Content: openai.Str("a")},
			{Role: "user", Content: openai.Str("b")},
		},
	}
	out := toInvoke(req)
	if len(out.Messages) != 1 || len(out.Messages[0].Content) != 2 {
		t.Fatalf("merge failed: %+v", out.Messages)
	}
}

func TestToInvokeAssistantToolCalls(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", ToolCalls: []openai.ToolCall{
				{ID: "tc1", Function: openai.FunctionCall{Name: "f", Arguments: `{"x":1}`}},
			}},
		},
	}
	out := toInvoke(req)
	b := out.Messages[0].Content[0]
	if b.Type != "tool_use" || b.ID != "tc1" || b.Name != "f" || string(b.Input) != `{"x":1}` {
		t.Fatalf("block=%+v", b)
	}
}

func TestToInvokeToolResult(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "tool", ToolCallID: "tc9", Content: openai.Str("res")},
		},
	}
	out := toInvoke(req)
	if out.Messages[0].Role != "user" {
		t.Fatalf("role=%q", out.Messages[0].Role)
	}
	b := out.Messages[0].Content[0]
	if b.Type != "tool_result" || b.ToolUseID != "tc9" || string(b.Content) != `"res"` {
		t.Fatalf("block=%+v", b)
	}
}

func TestToInvokeTools(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Tools: []openai.Tool{
			{Type: "function", Function: openai.FunctionDef{Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "other", Function: openai.FunctionDef{Name: "skip"}},
		},
	}
	out := toInvoke(req)
	if len(out.Tools) != 1 || out.Tools[0].Name != "f" {
		t.Fatalf("tools=%+v", out.Tools)
	}
}

// --- imageBlock -------------------------------------------------------------

func TestImageBlock(t *testing.T) {
	b := imageBlock("data:image/png;base64,QUJD")
	if b.Source == nil || b.Source.Type != "base64" || b.Source.MediaType != "image/png" || b.Source.Data != "QUJD" {
		t.Fatalf("data uri source=%+v", b.Source)
	}
	b = imageBlock("https://x/y.png")
	if b.Source.Type != "url" || b.Source.URL != "https://x/y.png" {
		t.Fatalf("url source=%+v", b.Source)
	}
}

func TestContentToBlocks(t *testing.T) {
	if got := contentToBlocks(openai.Str("")); got != nil {
		t.Fatalf("empty -> %+v", got)
	}
	multi := openai.MessageContent{Parts: []openai.ContentPart{
		{Type: "text", Text: "t"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "data:image/png;base64,AA"}},
		{Type: "image_url"}, // nil skipped
	}}
	blocks := contentToBlocks(multi)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%+v", blocks)
	}
}

// --- convertToolChoice ------------------------------------------------------
//
// NOTE: bedrock's convertToolChoice differs from anthropic's: it has no explicit
// "none" case and falls into the default branch (returns nil). It also returns
// nil for any unknown string. Both anthropic and bedrock map "none" -> nil, so
// behavior is equivalent here even though the code paths differ.

func TestConvertToolChoice(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", ``, ""},
		{"auto", `"auto"`, `{"type":"auto"}`},
		{"required", `"required"`, `{"type":"any"}`},
		{"none", `"none"`, ""},
		{"unknown string", `"bogus"`, ""},
		{"function object", `{"type":"function","function":{"name":"myfn"}}`, `{"name":"myfn","type":"tool"}`},
		{"object no name", `{"type":"function","function":{}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToolChoice(json.RawMessage(tt.in))
			if tt.want == "" {
				if got != nil {
					t.Fatalf("got %s want nil", got)
				}
				return
			}
			if !jsonEqual(t, got, []byte(tt.want)) {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

// --- fromInvoke -------------------------------------------------------------

func TestFromInvoke(t *testing.T) {
	resp := &invokeResponse{
		ID:    "id",
		Model: "",
		Content: []block{
			{Type: "text", Text: "a"},
			{Type: "text", Text: "b"},
			{Type: "tool_use", ID: "t", Name: "fn", Input: json.RawMessage(`{}`)},
		},
		StopReason: "max_tokens",
		Usage:      usage{InputTokens: 2, OutputTokens: 4},
	}
	out := fromInvoke(resp, "requested-model")
	c := out.Choices[0]
	if c.Message.Content.String() != "ab" {
		t.Fatalf("content=%q", c.Message.Content.String())
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("toolcalls=%+v", c.Message.ToolCalls)
	}
	// Note: stop_reason is max_tokens -> length, even though a tool_use block exists.
	if c.FinishReason != "length" {
		t.Fatalf("finish=%q", c.FinishReason)
	}
	if out.Model != "requested-model" {
		t.Fatalf("model fallback=%q", out.Model)
	}
	if out.Usage.TotalTokens != 6 {
		t.Fatalf("total=%d", out.Usage.TotalTokens)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"":              "",
		"unknown":       "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q)=%q want %q", in, got, want)
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

func TestJSONString(t *testing.T) {
	if got := string(jsonString("x")); got != `"x"` {
		t.Fatalf("jsonString=%s", got)
	}
}

func TestBedrockImageBlockBase64Marker(t *testing.T) {
	// With ;base64 marker -> base64 source.
	b := imageBlock("data:image/png;base64,QUJD")
	if b.Source == nil || b.Source.Type != "base64" {
		t.Fatalf("base64 marker: source=%+v want type base64", b.Source)
	}
	if b.Source.MediaType != "image/png" || b.Source.Data != "QUJD" {
		t.Errorf("base64 marker: media=%q data=%q", b.Source.MediaType, b.Source.Data)
	}
}

func TestBedrockImageBlockNoBase64Marker(t *testing.T) {
	// data: URI WITHOUT ;base64 must NOT be labeled base64.
	b := imageBlock("data:image/png,RAWBYTES")
	if b.Source == nil {
		t.Fatal("nil source")
	}
	if b.Source.Type == "base64" {
		t.Errorf("data URI without ;base64 was wrongly labeled base64: %+v", b.Source)
	}
	if b.Source.MediaType != "image/png" || b.Source.Data != "RAWBYTES" {
		t.Errorf("media=%q data=%q", b.Source.MediaType, b.Source.Data)
	}
}

func TestBedrockImageBlockPlainURL(t *testing.T) {
	b := imageBlock("https://example.com/cat.png")
	if b.Source == nil || b.Source.Type != "url" || b.Source.URL != "https://example.com/cat.png" {
		t.Errorf("plain url: source=%+v", b.Source)
	}
}

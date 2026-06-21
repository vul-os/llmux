package anthropic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/openai"
)

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(i int) *int           { return &i }

// --- max_tokens defaulting --------------------------------------------------

func TestToAnthropicMaxTokens(t *testing.T) {
	tests := []struct {
		name string
		req  *openai.ChatCompletionRequest
		want int
	}{
		{"default", &openai.ChatCompletionRequest{}, defaultMaxTokens},
		{"max_tokens", &openai.ChatCompletionRequest{MaxTokens: ptrInt(10)}, 10},
		{"max_completion_tokens", &openai.ChatCompletionRequest{MaxCompletionTokens: ptrInt(25)}, 25},
		{"max_tokens wins over completion", &openai.ChatCompletionRequest{MaxTokens: ptrInt(7), MaxCompletionTokens: ptrInt(99)}, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := toAnthropic(tt.req, "m")
			if err != nil {
				t.Fatal(err)
			}
			if out.MaxTokens != tt.want {
				t.Fatalf("MaxTokens=%d want %d", out.MaxTokens, tt.want)
			}
		})
	}
}

// --- scalar passthrough -----------------------------------------------------

func TestToAnthropicScalars(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Temperature: ptrFloat(0.5),
		TopP:        ptrFloat(0.9),
		Stream:      true,
		Stop:        &openai.StringOrArray{Values: []string{"X", "Y"}},
	}
	out, err := toAnthropic(req, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-x" {
		t.Fatalf("Model=%q", out.Model)
	}
	if out.Temperature == nil || *out.Temperature != 0.5 {
		t.Fatalf("Temperature=%v", out.Temperature)
	}
	if out.TopP == nil || *out.TopP != 0.9 {
		t.Fatalf("TopP=%v", out.TopP)
	}
	if !out.Stream {
		t.Fatal("Stream not set")
	}
	if !reflect.DeepEqual(out.StopSequences, []string{"X", "Y"}) {
		t.Fatalf("StopSequences=%v", out.StopSequences)
	}
}

// --- system/developer extraction & merging ----------------------------------

func TestToAnthropicSystemExtraction(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("sys1")},
			{Role: "developer", Content: openai.Str("dev1")},
			{Role: "system", Content: openai.Str("")}, // empty dropped
			{Role: "user", Content: openai.Str("hi")},
		},
	}
	out, err := toAnthropic(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if out.System != "sys1\n\ndev1" {
		t.Fatalf("System=%q", out.System)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("messages=%+v", out.Messages)
	}
}

// --- same-role merging ------------------------------------------------------

func TestToAnthropicMergeSameRole(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "user", Content: openai.Str("a")},
			{Role: "user", Content: openai.Str("b")},
		},
	}
	out, _ := toAnthropic(req, "m")
	if len(out.Messages) != 1 {
		t.Fatalf("expected merge into 1 message, got %d", len(out.Messages))
	}
	if len(out.Messages[0].Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(out.Messages[0].Content))
	}
}

// --- assistant tool_calls -> tool_use ---------------------------------------

func TestToAnthropicAssistantToolCalls(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", Content: openai.Str("thinking"), ToolCalls: []openai.ToolCall{
				{ID: "tc1", Function: openai.FunctionCall{Name: "f", Arguments: `{"x":1}`}},
				{ID: "tc2", Function: openai.FunctionCall{Name: "g", Arguments: ``}},
			}},
		},
	}
	out, _ := toAnthropic(req, "m")
	blocks := out.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("expected text + 2 tool_use, got %d", len(blocks))
	}
	if blocks[0].Type != "text" {
		t.Fatalf("block0=%s", blocks[0].Type)
	}
	if blocks[1].Type != "tool_use" || blocks[1].ID != "tc1" || blocks[1].Name != "f" {
		t.Fatalf("block1=%+v", blocks[1])
	}
	if string(blocks[1].Input) != `{"x":1}` {
		t.Fatalf("input=%s", blocks[1].Input)
	}
	// empty args default to {}
	if string(blocks[2].Input) != `{}` {
		t.Fatalf("empty args input=%s", blocks[2].Input)
	}
}

// --- tool result mapping ----------------------------------------------------

func TestToAnthropicToolResult(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "tool", ToolCallID: "tc1", Content: openai.Str("the result")},
		},
	}
	out, _ := toAnthropic(req, "m")
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("tool result should map to user role: %+v", out.Messages)
	}
	b := out.Messages[0].Content[0]
	if b.Type != "tool_result" || b.ToolUseID != "tc1" {
		t.Fatalf("block=%+v", b)
	}
	if string(b.Content) != `"the result"` {
		t.Fatalf("content=%s", b.Content)
	}
}

// --- tools -> declarations --------------------------------------------------

func TestToAnthropicTools(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Tools: []openai.Tool{
			{Type: "function", Function: openai.FunctionDef{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "code_interpreter", Function: openai.FunctionDef{Name: "skip"}}, // non-function skipped
			{Function: openai.FunctionDef{Name: "g"}},                              // empty type -> kept
		},
	}
	out, _ := toAnthropic(req, "m")
	if len(out.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %+v", len(out.Tools), out.Tools)
	}
	if out.Tools[0].Name != "f" || out.Tools[0].Description != "d" {
		t.Fatalf("tool0=%+v", out.Tools[0])
	}
	if string(out.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("schema=%s", out.Tools[0].InputSchema)
	}
	// empty params default to {}
	if string(out.Tools[1].InputSchema) != `{}` {
		t.Fatalf("g schema=%s", out.Tools[1].InputSchema)
	}
}

// --- content blocks ---------------------------------------------------------

func TestContentToBlocks(t *testing.T) {
	if got := contentToBlocks(openai.Str("")); got != nil {
		t.Fatalf("empty string should yield nil, got %+v", got)
	}
	got := contentToBlocks(openai.Str("hi"))
	if len(got) != 1 || got[0].Type != "text" || got[0].Text != "hi" {
		t.Fatalf("string content=%+v", got)
	}

	multi := openai.MessageContent{Parts: []openai.ContentPart{
		{Type: "text", Text: "t"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "https://x/y.png"}},
		{Type: "image_url"}, // nil ImageURL skipped
		{Type: "input_audio"},
	}}
	blocks := contentToBlocks(multi)
	if len(blocks) != 2 {
		t.Fatalf("expected text+image, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || blocks[1].Type != "image" {
		t.Fatalf("blocks=%+v", blocks)
	}
}

// --- imageBlock -------------------------------------------------------------

func TestImageBlock(t *testing.T) {
	t.Run("data uri base64", func(t *testing.T) {
		b := imageBlock("data:image/png;base64,QUJD")
		if b.Type != "image" || b.Source == nil {
			t.Fatalf("block=%+v", b)
		}
		if b.Source.Type != "base64" || b.Source.MediaType != "image/png" || b.Source.Data != "QUJD" {
			t.Fatalf("source=%+v", b.Source)
		}
	})
	t.Run("data uri no params", func(t *testing.T) {
		b := imageBlock("data:image/jpeg,RAW")
		if b.Source.MediaType != "image/jpeg" || b.Source.Data != "RAW" {
			t.Fatalf("source=%+v", b.Source)
		}
	})
	t.Run("plain url", func(t *testing.T) {
		b := imageBlock("https://example.com/a.png")
		if b.Source.Type != "url" || b.Source.URL != "https://example.com/a.png" {
			t.Fatalf("source=%+v", b.Source)
		}
	})
	t.Run("malformed data uri falls through to url", func(t *testing.T) {
		// no comma -> treated as url
		b := imageBlock("data:image/png;base64")
		if b.Source.Type != "url" {
			t.Fatalf("source=%+v", b.Source)
		}
	})
}

// --- convertToolChoice ------------------------------------------------------

func TestConvertToolChoice(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // "" means nil result
	}{
		{"empty", ``, ""},
		{"auto", `"auto"`, `{"type":"auto"}`},
		{"required", `"required"`, `{"type":"any"}`},
		{"none", `"none"`, `{"type":"none"}`},
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
			// compare canonicalized JSON (map marshaling key order varies)
			if !jsonEqual(t, got, []byte(tt.want)) {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

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

// --- fromAnthropic ----------------------------------------------------------

func TestFromAnthropic(t *testing.T) {
	resp := &messagesResponse{
		ID:    "id1",
		Model: "claude-real",
		Content: []block{
			{Type: "text", Text: "Hello "},
			{Type: "text", Text: "world"},
			{Type: "tool_use", ID: "t1", Name: "fn", Input: json.RawMessage(`{"a":1}`)},
		},
		StopReason: "tool_use",
		Usage:      usage{InputTokens: 3, OutputTokens: 5},
	}
	out := fromAnthropic(resp, "requested")
	c := out.Choices[0]
	if c.Message.Content.String() != "Hello world" {
		t.Fatalf("content=%q", c.Message.Content.String())
	}
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "fn" {
		t.Fatalf("toolcalls=%+v", c.Message.ToolCalls)
	}
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", c.FinishReason)
	}
	if out.Model != "claude-real" {
		t.Fatalf("model=%q", out.Model)
	}
	if out.Usage.TotalTokens != 8 {
		t.Fatalf("total=%d", out.Usage.TotalTokens)
	}
}

func TestFromAnthropicModelFallback(t *testing.T) {
	out := fromAnthropic(&messagesResponse{}, "fallback-model")
	if out.Model != "fallback-model" {
		t.Fatalf("model=%q", out.Model)
	}
}

// --- mapStopReason ----------------------------------------------------------

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"":              "",
		"weird":         "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q)=%q want %q", in, got, want)
		}
	}
}

// --- convertToolChoice "none" / mapStopReason "refusal" (correctness fixes) -

func TestConvertToolChoiceNoneExplicit(t *testing.T) {
	got := convertToolChoice(json.RawMessage(`"none"`))
	if got == nil {
		t.Fatalf("convertToolChoice(\"none\") returned nil; want explicit {\"type\":\"none\"}")
	}
	if !jsonEqual(t, got, []byte(`{"type":"none"}`)) {
		t.Fatalf("convertToolChoice(\"none\")=%s want {\"type\":\"none\"}", got)
	}
}

func TestMapStopReasonRefusal(t *testing.T) {
	if got := mapStopReason("refusal"); got != "content_filter" {
		t.Errorf("mapStopReason(\"refusal\")=%q want %q", got, "content_filter")
	}
	// Existing reasons must be unaffected.
	cases := map[string]string{
		"end_turn":   "stop",
		"max_tokens": "length",
		"tool_use":   "tool_calls",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q)=%q want %q", in, got, want)
		}
	}
}

// --- rawOrEmptyObject -------------------------------------------------------

func TestRawOrEmptyObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `{}`},
		{"   ", `{}`},
		{`{"a":1}`, `{"a":1}`},
		{"not json", `{}`},
		{`[1,2]`, `[1,2]`}, // valid json (not necessarily object) passes through
	}
	for _, c := range cases {
		if got := string(rawOrEmptyObject(c.in)); got != c.want {
			t.Errorf("rawOrEmptyObject(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestJSONString(t *testing.T) {
	if got := string(jsonString(`he said "hi"`)); got != `"he said \"hi\""` {
		t.Fatalf("jsonString=%s", got)
	}
}

// --- response_format (universal JSON fallback) ------------------------------

func TestToAnthropicResponseFormatJSONObject(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		ResponseFormat: &openai.ResponseFormat{Type: "json_object"},
		Messages:       []openai.Message{{Role: "system", Content: openai.Str("be brief")}},
	}
	out, err := toAnthropic(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.System, "be brief") {
		t.Errorf("existing system dropped: %q", out.System)
	}
	if !strings.Contains(out.System, "valid JSON only") {
		t.Errorf("missing JSON instruction: %q", out.System)
	}
}

func TestToAnthropicResponseFormatJSONSchema(t *testing.T) {
	wrapper := `{"name":"thing","strict":true,"schema":{"type":"object","properties":{"a":{"type":"string"}}}}`
	req := &openai.ChatCompletionRequest{
		ResponseFormat: &openai.ResponseFormat{Type: "json_schema", JSONSchema: json.RawMessage(wrapper)},
	}
	out, err := toAnthropic(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.System, "valid JSON only") {
		t.Errorf("missing JSON instruction: %q", out.System)
	}
	if !strings.Contains(out.System, `"properties"`) || !strings.Contains(out.System, `"a"`) {
		t.Errorf("schema not embedded: %q", out.System)
	}
	if strings.Contains(out.System, "strict") {
		t.Errorf("strict wrapper leaked into instruction: %q", out.System)
	}
}

func TestToAnthropicResponseFormatTextNoChange(t *testing.T) {
	req := &openai.ChatCompletionRequest{ResponseFormat: &openai.ResponseFormat{Type: "text"}}
	out, err := toAnthropic(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if out.System != "" {
		t.Errorf("text response_format should add no instruction, got %q", out.System)
	}
}

// --- parallel_tool_calls -> disable_parallel_tool_use -----------------------

func TestToAnthropicParallelToolCalls(t *testing.T) {
	tru := true
	fls := false
	tests := []struct {
		name        string
		parallel    *bool
		toolChoice  json.RawMessage
		wantPresent bool
		wantDisable bool
		wantType    string
	}{
		{"nil leaves choice nil", nil, nil, false, false, ""},
		{"false defaults to auto + disable true", &fls, nil, true, true, "auto"},
		{"true defaults to auto + disable false", &tru, nil, true, false, "auto"},
		{"false preserves explicit choice", &fls, json.RawMessage(`"required"`), true, true, "any"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &openai.ChatCompletionRequest{ParallelToolCalls: tt.parallel, ToolChoice: tt.toolChoice}
			out, err := toAnthropic(req, "m")
			if err != nil {
				t.Fatal(err)
			}
			if !tt.wantPresent {
				if out.ToolChoice != nil {
					t.Fatalf("tool_choice=%s want nil", out.ToolChoice)
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(out.ToolChoice, &m); err != nil {
				t.Fatalf("tool_choice not an object: %s", out.ToolChoice)
			}
			if m["type"] != tt.wantType {
				t.Errorf("type=%v want %q", m["type"], tt.wantType)
			}
			if got, _ := m["disable_parallel_tool_use"].(bool); got != tt.wantDisable {
				t.Errorf("disable_parallel_tool_use=%v want %v", m["disable_parallel_tool_use"], tt.wantDisable)
			}
		})
	}
}

// --- user -> metadata.user_id -----------------------------------------------

func TestToAnthropicUserMetadata(t *testing.T) {
	req := &openai.ChatCompletionRequest{User: "user-42"}
	out, err := toAnthropic(req, "m")
	if err != nil {
		t.Fatal(err)
	}
	if out.Metadata == nil || out.Metadata.UserID != "user-42" {
		t.Fatalf("metadata=%+v want user_id=user-42", out.Metadata)
	}

	noUser := &openai.ChatCompletionRequest{}
	out2, _ := toAnthropic(noUser, "m")
	if out2.Metadata != nil {
		t.Errorf("metadata should be nil when User empty, got %+v", out2.Metadata)
	}
}

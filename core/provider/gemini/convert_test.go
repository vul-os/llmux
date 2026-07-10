package gemini

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
		{"empty raw", ``, nil, true},
		{"null", `null`, nil, true},
		{"whitespace", `   `, nil, true},
		{"number not allowed", `42`, nil, true},
		{"object not allowed", `{"x":1}`, nil, true},
		{"array of numbers", `[1,2]`, nil, true},
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
				t.Fatalf("unexpected error: %v", err)
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
		if !strings.Contains(err.Error(), "token-array embedding input is not supported by Gemini") {
			t.Fatalf("input %s: unclear error: %v", in, err)
		}
	}
	// String and []string still work.
	if _, err := decodeEmbeddingInput(json.RawMessage(`"hi"`)); err != nil {
		t.Fatalf("string input should work: %v", err)
	}
	if _, err := decodeEmbeddingInput(json.RawMessage(`["a","b"]`)); err != nil {
		t.Fatalf("[]string input should work: %v", err)
	}
}

// --- toGemini config --------------------------------------------------------

func TestToGeminiGenerationConfig(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Temperature: ptrFloat(0.4),
		TopP:        ptrFloat(0.7),
		MaxTokens:   ptrInt(50),
		Stop:        &openai.StringOrArray{Values: []string{"Z"}},
	}
	out := toGemini(req)
	cfg := out.GenerationConfig
	if cfg.Temperature == nil || *cfg.Temperature != 0.4 {
		t.Fatalf("temp=%v", cfg.Temperature)
	}
	if cfg.TopP == nil || *cfg.TopP != 0.7 {
		t.Fatalf("topp=%v", cfg.TopP)
	}
	if cfg.MaxOutputTokens == nil || *cfg.MaxOutputTokens != 50 {
		t.Fatalf("maxout=%v", cfg.MaxOutputTokens)
	}
	if !reflect.DeepEqual(cfg.StopSequences, []string{"Z"}) {
		t.Fatalf("stop=%v", cfg.StopSequences)
	}
}

func TestToGeminiMaxCompletionTokens(t *testing.T) {
	out := toGemini(&openai.ChatCompletionRequest{MaxCompletionTokens: ptrInt(33)})
	if out.GenerationConfig.MaxOutputTokens == nil || *out.GenerationConfig.MaxOutputTokens != 33 {
		t.Fatalf("maxout=%v", out.GenerationConfig.MaxOutputTokens)
	}
}

func TestToGeminiNoMaxTokens(t *testing.T) {
	out := toGemini(&openai.ChatCompletionRequest{})
	if out.GenerationConfig.MaxOutputTokens != nil {
		t.Fatalf("expected nil maxout, got %v", *out.GenerationConfig.MaxOutputTokens)
	}
}

// --- system instruction & roles ---------------------------------------------

func TestToGeminiSystemInstruction(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("s1")},
			{Role: "developer", Content: openai.Str("d1")},
			{Role: "user", Content: openai.Str("hi")},
		},
	}
	out := toGemini(req)
	if out.SystemInstruction == nil {
		t.Fatal("expected system instruction")
	}
	if out.SystemInstruction.Parts[0].Text != "s1\n\nd1" {
		t.Fatalf("sysinstr=%q", out.SystemInstruction.Parts[0].Text)
	}
	if len(out.Contents) != 1 || out.Contents[0].Role != "user" {
		t.Fatalf("contents=%+v", out.Contents)
	}
}

func TestToGeminiNoSystem(t *testing.T) {
	out := toGemini(&openai.ChatCompletionRequest{
		Messages: []openai.Message{{Role: "user", Content: openai.Str("x")}},
	})
	if out.SystemInstruction != nil {
		t.Fatalf("expected nil system instruction, got %+v", out.SystemInstruction)
	}
}

func TestToGeminiAssistantRoleAndMerge(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", Content: openai.Str("first")},
			{Role: "assistant", Content: openai.Str("second")},
		},
	}
	out := toGemini(req)
	if len(out.Contents) != 1 || out.Contents[0].Role != "model" {
		t.Fatalf("expected merged model turn: %+v", out.Contents)
	}
	if len(out.Contents[0].Parts) != 2 {
		t.Fatalf("parts=%+v", out.Contents[0].Parts)
	}
}

func TestToGeminiAssistantFunctionCall(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", ToolCalls: []openai.ToolCall{
				{ID: "tc", Function: openai.FunctionCall{Name: "f", Arguments: `{"a":1}`}},
			}},
		},
	}
	out := toGemini(req)
	p := out.Contents[0].Parts[0]
	if p.FunctionCall == nil || p.FunctionCall.Name != "f" {
		t.Fatalf("fncall=%+v", p.FunctionCall)
	}
	if string(p.FunctionCall.Args) != `{"a":1}` {
		t.Fatalf("args=%s", p.FunctionCall.Args)
	}
}

func TestToGeminiToolResponse(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "tool", Name: "myfn", Content: openai.Str("plain result")},
		},
	}
	out := toGemini(req)
	if out.Contents[0].Role != "user" {
		t.Fatalf("role=%q", out.Contents[0].Role)
	}
	fr := out.Contents[0].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "myfn" {
		t.Fatalf("fnresp=%+v", fr)
	}
	if string(fr.Response) != `{"result":"plain result"}` {
		t.Fatalf("response=%s", fr.Response)
	}
}

func TestToGeminiTools(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Tools: []openai.Tool{
			{Type: "function", Function: openai.FunctionDef{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "retrieval", Function: openai.FunctionDef{Name: "skip"}},
			{Function: openai.FunctionDef{Name: "g"}},
		},
	}
	out := toGemini(req)
	if len(out.Tools) != 1 {
		t.Fatalf("expected single tools entry, got %d", len(out.Tools))
	}
	decls := out.Tools[0].FunctionDeclarations
	if len(decls) != 2 {
		t.Fatalf("expected 2 decls, got %d", len(decls))
	}
	if decls[0].Name != "f" || decls[0].Description != "d" {
		t.Fatalf("decl0=%+v", decls[0])
	}
	// g has no params -> cleanSchema default
	if string(decls[1].Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("g params=%s", decls[1].Parameters)
	}
}

// --- contentToParts / dataURIToInline ---------------------------------------

func TestContentToParts(t *testing.T) {
	if got := contentToParts(openai.Str("")); got != nil {
		t.Fatalf("empty -> %+v", got)
	}
	got := contentToParts(openai.Str("hi"))
	if len(got) != 1 || got[0].Text != "hi" {
		t.Fatalf("string -> %+v", got)
	}

	multi := openai.MessageContent{Parts: []openai.ContentPart{
		{Type: "text", Text: "t"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "data:image/png;base64,AA"}},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "https://x/y.png"}}, // plain url skipped
		{Type: "image_url"}, // nil skipped
	}}
	parts := contentToParts(multi)
	if len(parts) != 2 {
		t.Fatalf("expected text + inline (plain url skipped), got %d: %+v", len(parts), parts)
	}
	if parts[0].Text != "t" || parts[1].InlineData == nil {
		t.Fatalf("parts=%+v", parts)
	}
}

func TestDataURIToInline(t *testing.T) {
	t.Run("base64 data uri", func(t *testing.T) {
		d := dataURIToInline("data:image/png;base64,QUJD")
		if d == nil || d.MimeType != "image/png" || d.Data != "QUJD" {
			t.Fatalf("inline=%+v", d)
		}
	})
	t.Run("no params", func(t *testing.T) {
		d := dataURIToInline("data:image/jpeg,RAW")
		if d == nil || d.MimeType != "image/jpeg" || d.Data != "RAW" {
			t.Fatalf("inline=%+v", d)
		}
	})
	t.Run("plain url nil", func(t *testing.T) {
		if d := dataURIToInline("https://x/y.png"); d != nil {
			t.Fatalf("expected nil, got %+v", d)
		}
	})
	t.Run("malformed no comma nil", func(t *testing.T) {
		if d := dataURIToInline("data:image/png;base64"); d != nil {
			t.Fatalf("expected nil, got %+v", d)
		}
	})
}

// --- wrapToolResult ---------------------------------------------------------

func TestWrapToolResult(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain string", "hello", `{"result":"hello"}`},
		{"json object passthrough", `{"a":1}`, `{"a":1}`},
		{"json object with whitespace", `  {"a":1}  `, `{"a":1}`},
		{"json array wrapped (not object)", `[1,2]`, `{"result":"[1,2]"}`},
		{"invalid object brace wrapped", `{bad`, `{"result":"{bad"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(wrapToolResult(tt.in)); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}

// --- fromGemini & mapFinishReason -------------------------------------------

func TestFromGeminiText(t *testing.T) {
	resp := &generateResponse{
		Candidates: []candidate{{
			Content: content{Parts: []part{
				{Text: "Hello "},
				{Text: "world"},
			}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &usageMetadata{PromptTokenCount: 3, CandidatesTokenCount: 5, TotalTokenCount: 8},
	}
	out := fromGemini(resp, "gemini-x", "id1", 123)
	c := out.Choices[0]
	if c.Message.Content.String() != "Hello world" {
		t.Fatalf("content=%q", c.Message.Content.String())
	}
	if c.FinishReason != "stop" {
		t.Fatalf("finish=%q", c.FinishReason)
	}
	if out.Usage == nil || out.Usage.TotalTokens != 8 {
		t.Fatalf("usage=%+v", out.Usage)
	}
	if out.Created != 123 || out.ID != "id1" || out.Model != "gemini-x" {
		t.Fatalf("meta wrong: %+v", out)
	}
}

func TestFromGeminiFunctionCall(t *testing.T) {
	resp := &generateResponse{
		Candidates: []candidate{{
			Content: content{Parts: []part{
				{FunctionCall: &fnCall{Name: "f", Args: json.RawMessage(`{"x":1}`)}},
			}},
			FinishReason: "STOP",
		}},
	}
	out := fromGemini(resp, "m", "id", 0)
	c := out.Choices[0]
	if len(c.Message.ToolCalls) != 1 || c.Message.ToolCalls[0].Function.Name != "f" {
		t.Fatalf("toolcalls=%+v", c.Message.ToolCalls)
	}
	if c.Message.ToolCalls[0].ID == "" {
		t.Fatal("expected generated tool call ID")
	}
	// finish overridden to tool_calls when tool calls present
	if c.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", c.FinishReason)
	}
}

func TestFromGeminiNoCandidates(t *testing.T) {
	out := fromGemini(&generateResponse{}, "m", "id", 0)
	if out.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish=%q", out.Choices[0].FinishReason)
	}
	if out.Usage != nil {
		t.Fatalf("expected nil usage, got %+v", out.Usage)
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		r        string
		hasTools bool
		want     string
	}{
		{"STOP", false, "stop"},
		{"", false, "stop"},
		{"MAX_TOKENS", false, "length"},
		{"SAFETY", false, "content_filter"},
		{"RECITATION", false, "content_filter"},
		{"BLOCKLIST", false, "content_filter"},
		{"PROHIBITED_CONTENT", false, "content_filter"},
		{"OTHER", false, "content_filter"},
		{"UNKNOWN_REASON", false, "stop"},
		{"MAX_TOKENS", true, "tool_calls"}, // hasTools overrides everything
		{"SAFETY", true, "tool_calls"},
	}
	for _, tt := range tests {
		if got := mapFinishReason(tt.r, tt.hasTools); got != tt.want {
			t.Errorf("mapFinishReason(%q,%v)=%q want %q", tt.r, tt.hasTools, got, tt.want)
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

func TestCleanSchema(t *testing.T) {
	if got := string(cleanSchema(nil)); got != `{"type":"object","properties":{}}` {
		t.Fatalf("nil -> %s", got)
	}
	if got := string(cleanSchema(json.RawMessage(`{"type":"string"}`))); got != `{"type":"string"}` {
		t.Fatalf("passthrough -> %s", got)
	}
}

// --- schema sanitization ----------------------------------------------------

// unmarshalSchema parses a cleaned schema for structural assertions that don't
// depend on JSON key ordering.
func unmarshalSchema(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("cleaned schema is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

func TestCleanSchemaStripsRejectedKeys(t *testing.T) {
	in := json.RawMessage(`{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"$id": "x",
		"$comment": "c",
		"title": "Args",
		"strict": true,
		"type": "object",
		"additionalProperties": false,
		"default": {},
		"examples": [{}],
		"properties": {
			"name": {
				"type": "string",
				"title": "Name",
				"default": "bob",
				"additionalProperties": false
			}
		}
	}`)
	m := unmarshalSchema(t, cleanSchema(in))
	for _, k := range []string{"$schema", "$id", "$comment", "title", "strict", "additionalProperties", "default", "examples"} {
		if _, ok := m[k]; ok {
			t.Errorf("top-level key %q should have been removed", k)
		}
	}
	if m["type"] != "object" {
		t.Errorf("type=%v want object", m["type"])
	}
	props := m["properties"].(map[string]any)
	name := props["name"].(map[string]any)
	for _, k := range []string{"title", "default", "additionalProperties"} {
		if _, ok := name[k]; ok {
			t.Errorf("nested key %q should have been removed", k)
		}
	}
	if name["type"] != "string" {
		t.Errorf("nested type=%v want string", name["type"])
	}
}

func TestCleanSchemaTypeArrayToNullable(t *testing.T) {
	in := json.RawMessage(`{"type":["string","null"]}`)
	m := unmarshalSchema(t, cleanSchema(in))
	if m["type"] != "string" {
		t.Errorf("type=%v want string", m["type"])
	}
	if m["nullable"] != true {
		t.Errorf("nullable=%v want true", m["nullable"])
	}
}

func TestCleanSchemaArrayGetsItems(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"tags":{"type":"array"}}}`)
	m := unmarshalSchema(t, cleanSchema(in))
	tags := m["properties"].(map[string]any)["tags"].(map[string]any)
	items, ok := tags["items"].(map[string]any)
	if !ok {
		t.Fatalf("array missing items: %v", tags)
	}
	if items["type"] != "string" {
		t.Errorf("default items type=%v want string", items["type"])
	}
}

func TestCleanSchemaFormatHandling(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{
		"a":{"type":"string","format":"email"},
		"b":{"type":"string","format":"date-time"},
		"c":{"type":"string","format":"enum"}
	}}`)
	props := unmarshalSchema(t, cleanSchema(in))["properties"].(map[string]any)
	if _, ok := props["a"].(map[string]any)["format"]; ok {
		t.Errorf("format=email should be dropped")
	}
	if props["b"].(map[string]any)["format"] != "date-time" {
		t.Errorf("format=date-time should be kept")
	}
	if props["c"].(map[string]any)["format"] != "enum" {
		t.Errorf("format=enum should be kept")
	}
}

func TestCleanSchemaRecursesAnyOfAndItems(t *testing.T) {
	in := json.RawMessage(`{
		"type":"object",
		"properties":{
			"x":{"anyOf":[{"type":"string","title":"drop"},{"type":["integer","null"]}]},
			"y":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"z":{"type":"array"}}}}
		}
	}`)
	props := unmarshalSchema(t, cleanSchema(in))["properties"].(map[string]any)

	anyOf := props["x"].(map[string]any)["anyOf"].([]any)
	first := anyOf[0].(map[string]any)
	if _, ok := first["title"]; ok {
		t.Errorf("anyOf branch should have title removed")
	}
	second := anyOf[1].(map[string]any)
	if second["type"] != "integer" || second["nullable"] != true {
		t.Errorf("anyOf type-array branch not normalized: %v", second)
	}

	items := props["y"].(map[string]any)["items"].(map[string]any)
	if _, ok := items["additionalProperties"]; ok {
		t.Errorf("items should have additionalProperties removed")
	}
	nestedArr := items["properties"].(map[string]any)["z"].(map[string]any)
	if _, ok := nestedArr["items"]; !ok {
		t.Errorf("deeply nested array should get default items")
	}
}

func TestCleanSchemaResolvesRefsAgainstDefs(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"user": {"$ref": "#/$defs/User"}
		},
		"$defs": {
			"User": {
				"type": "object",
				"title": "User",
				"properties": {
					"name": {"type": "string"},
					"address": {"$ref": "#/$defs/Address"}
				}
			},
			"Address": {
				"type": "object",
				"properties": {
					"city": {"type": "string"}
				}
			}
		}
	}`)
	m := unmarshalSchema(t, cleanSchema(in))

	// $defs must not survive into the Gemini-facing schema.
	if _, ok := m["$defs"]; ok {
		t.Fatalf("$defs leaked into cleaned schema: %v", m)
	}

	user, ok := m["properties"].(map[string]any)["user"].(map[string]any)
	if !ok {
		t.Fatalf("user property missing/wrong shape: %v", m)
	}
	if _, ok := user["$ref"]; ok {
		t.Fatalf("$ref left unresolved on user: %v", user)
	}
	if user["type"] != "object" {
		t.Fatalf("user.type=%v want object (inlined from $defs.User)", user["type"])
	}
	if _, ok := user["title"]; ok {
		t.Errorf("inlined ref content should still be sanitized (title dropped): %v", user)
	}
	name, ok := user["properties"].(map[string]any)["name"].(map[string]any)
	if !ok || name["type"] != "string" {
		t.Fatalf("user.properties.name not inlined correctly: %v", user)
	}

	// Nested $ref (User.address -> Address) must also be resolved, recursively.
	addr, ok := user["properties"].(map[string]any)["address"].(map[string]any)
	if !ok {
		t.Fatalf("address property missing after nested ref resolution: %v", user)
	}
	if _, ok := addr["$ref"]; ok {
		t.Fatalf("nested $ref left unresolved: %v", addr)
	}
	city, ok := addr["properties"].(map[string]any)["city"].(map[string]any)
	if !ok || city["type"] != "string" {
		t.Fatalf("address.properties.city not inlined correctly: %v", addr)
	}
}

func TestCleanSchemaResolvesLegacyDefinitionsKeyword(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {"item": {"$ref": "#/definitions/Item"}},
		"definitions": {"Item": {"type": "string"}}
	}`)
	m := unmarshalSchema(t, cleanSchema(in))
	if _, ok := m["definitions"]; ok {
		t.Fatalf("definitions leaked into cleaned schema: %v", m)
	}
	item, ok := m["properties"].(map[string]any)["item"].(map[string]any)
	if !ok || item["type"] != "string" {
		t.Fatalf("item not inlined from legacy definitions: %v", m)
	}
}

func TestCleanSchemaUnresolvableRefFallsBackToOpenObject(t *testing.T) {
	// A $ref with no matching $defs entry must not be forwarded to Gemini
	// verbatim (a dangling $ref is exactly what Gemini rejects).
	in := json.RawMessage(`{
		"type": "object",
		"properties": {"x": {"$ref": "#/$defs/Missing"}}
	}`)
	m := unmarshalSchema(t, cleanSchema(in))
	x, ok := m["properties"].(map[string]any)["x"].(map[string]any)
	if !ok {
		t.Fatalf("x property missing: %v", m)
	}
	if _, ok := x["$ref"]; ok {
		t.Fatalf("unresolvable $ref must not survive: %v", x)
	}
}

// TestToGeminiToolsWithRefSchema is an end-to-end check via toGemini (not just
// cleanSchema directly) that a tool parameters schema using $ref/$defs — the
// TODO this test targets — converts to a usable, ref-free Gemini declaration
// instead of forwarding a schema Gemini would 400 on.
func TestToGeminiToolsWithRefSchema(t *testing.T) {
	params := json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {"$ref": "#/$defs/Target"}
		},
		"required": ["target"],
		"$defs": {
			"Target": {
				"type": "object",
				"properties": {
					"city": {"type": "string"},
					"country": {"type": "string"}
				}
			}
		}
	}`)
	req := &openai.ChatCompletionRequest{
		Tools: []openai.Tool{
			{Type: "function", Function: openai.FunctionDef{Name: "get_weather", Description: "d", Parameters: params}},
		},
	}
	out := toGemini(req)
	if len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("expected one tool decl, got %+v", out.Tools)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Tools[0].FunctionDeclarations[0].Parameters, &parsed); err != nil {
		t.Fatalf("decl parameters not valid JSON: %v", err)
	}
	if _, ok := parsed["$defs"]; ok {
		t.Fatalf("$defs leaked into tool declaration: %s", out.Tools[0].FunctionDeclarations[0].Parameters)
	}
	target, ok := parsed["properties"].(map[string]any)["target"].(map[string]any)
	if !ok {
		t.Fatalf("target property missing/unresolved: %s", out.Tools[0].FunctionDeclarations[0].Parameters)
	}
	if _, ok := target["$ref"]; ok {
		t.Fatalf("tool schema $ref left unresolved: %v", target)
	}
	if target["type"] != "object" {
		t.Fatalf("target.type=%v want object", target["type"])
	}
}

func TestCleanSchemaInvalidFallsBack(t *testing.T) {
	for _, in := range []string{`not json`, `[1,2,3]`, `"a string"`, `42`} {
		if got := string(cleanSchema(json.RawMessage(in))); got != `{"type":"object","properties":{}}` {
			t.Errorf("cleanSchema(%q)=%s want fallback", in, got)
		}
	}
}

// --- functionResponse name recovery -----------------------------------------

func TestToGeminiToolResponseNameRecovery(t *testing.T) {
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "assistant", ToolCalls: []openai.ToolCall{
				{ID: "call_123", Type: "function", Function: openai.FunctionCall{Name: "get_weather", Arguments: `{}`}},
			}},
			// tool result omits Name, carries only the tool_call_id.
			{Role: "tool", ToolCallID: "call_123", Content: openai.Str("sunny")},
		},
	}
	out := toGemini(req)
	// out.Contents[0] is the model turn; [1] is the recovered tool response.
	var fr *fnResponse
	for _, c := range out.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				fr = p.FunctionResponse
			}
		}
	}
	if fr == nil {
		t.Fatal("expected a functionResponse part")
	}
	if fr.Name != "get_weather" {
		t.Fatalf("name=%q want get_weather (recovered from tool_call_id)", fr.Name)
	}
}

func TestToGeminiToolResponseNameUnrecoverable(t *testing.T) {
	// No matching assistant tool_call: name stays empty (nothing to recover).
	req := &openai.ChatCompletionRequest{
		Messages: []openai.Message{
			{Role: "tool", ToolCallID: "missing", Content: openai.Str("x")},
		},
	}
	out := toGemini(req)
	fr := out.Contents[0].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "" {
		t.Fatalf("expected empty name, got %+v", fr)
	}
}

func TestMapFinishReasonContentFilterAdditions(t *testing.T) {
	for _, r := range []string{"SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "LANGUAGE", "OTHER"} {
		if got := mapFinishReason(r, false); got != "content_filter" {
			t.Errorf("mapFinishReason(%q)=%q want content_filter", r, got)
		}
	}
}

// --- response_format mapping ------------------------------------------------

func TestToGeminiResponseFormatJSONObject(t *testing.T) {
	req := &openai.ChatCompletionRequest{ResponseFormat: &openai.ResponseFormat{Type: "json_object"}}
	out := toGemini(req)
	if out.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("mime=%q want application/json", out.GenerationConfig.ResponseMimeType)
	}
	if out.GenerationConfig.ResponseSchema != nil {
		t.Errorf("json_object should not set responseSchema: %s", out.GenerationConfig.ResponseSchema)
	}
}

func TestToGeminiResponseFormatJSONSchema(t *testing.T) {
	wrapper := `{"name":"thing","strict":true,"schema":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}}`
	req := &openai.ChatCompletionRequest{
		ResponseFormat: &openai.ResponseFormat{Type: "json_schema", JSONSchema: json.RawMessage(wrapper)},
	}
	out := toGemini(req)
	if out.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("mime=%q want application/json", out.GenerationConfig.ResponseMimeType)
	}
	schema := string(out.GenerationConfig.ResponseSchema)
	if schema == "" {
		t.Fatal("responseSchema not set")
	}
	// Sanitizer must strip rejected keys (strict, additionalProperties).
	if strings.Contains(schema, "additionalProperties") || strings.Contains(schema, "strict") {
		t.Errorf("schema not sanitized: %s", schema)
	}
	if !strings.Contains(schema, `"properties"`) {
		t.Errorf("schema body missing: %s", schema)
	}
}

func TestToGeminiResponseFormatTextNoChange(t *testing.T) {
	req := &openai.ChatCompletionRequest{ResponseFormat: &openai.ResponseFormat{Type: "text"}}
	out := toGemini(req)
	if out.GenerationConfig.ResponseMimeType != "" || out.GenerationConfig.ResponseSchema != nil {
		t.Errorf("text response_format must not change generationConfig: %+v", out.GenerationConfig)
	}
}

// --- empty tool-call args become {} (unary path) ----------------------------

func TestFromGeminiEmptyToolArgs(t *testing.T) {
	resp := &generateResponse{
		Candidates: []candidate{{
			Content:      content{Parts: []part{{FunctionCall: &fnCall{Name: "ping"}}}},
			FinishReason: "STOP",
		}},
	}
	out := fromGemini(resp, "g", "id", 0)
	tc := out.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Arguments != "{}" {
		t.Fatalf("args=%q want {}", tc[0].Function.Arguments)
	}
}

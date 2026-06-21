package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/llmux/llmux/core/openai"
)

// decodeEmbeddingInput decodes an OpenAI embeddings Input, which may be a JSON
// string or a JSON array of strings, into a slice of strings. A single string
// yields a one-element slice. Token-array inputs are not supported.
func decodeEmbeddingInput(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("embeddings input is empty")
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("invalid embeddings input string: %w", err)
		}
		return []string{s}, nil
	case '[':
		var ss []string
		if err := json.Unmarshal(trimmed, &ss); err != nil {
			if isTokenArrayInput(trimmed) {
				return nil, errTokenArrayInput("Gemini")
			}
			return nil, fmt.Errorf("embeddings input must be a string or array of strings: %w", err)
		}
		return ss, nil
	default:
		return nil, fmt.Errorf("embeddings input must be a string or array of strings")
	}
}

// isTokenArrayInput reports whether raw is an OpenAI token-id embedding input: a
// JSON array of numbers ([]int) or an array of arrays of numbers ([][]int).
func isTokenArrayInput(raw json.RawMessage) bool {
	var nums []json.Number
	if err := json.Unmarshal(raw, &nums); err == nil {
		return len(nums) > 0
	}
	var lists [][]json.Number
	if err := json.Unmarshal(raw, &lists); err == nil {
		return len(lists) > 0
	}
	return false
}

// errTokenArrayInput returns a clear, actionable error for token-array embedding
// inputs, which the named provider cannot accept natively.
func errTokenArrayInput(provider string) error {
	return fmt.Errorf("token-array embedding input is not supported by %s; "+
		"pass text input or route token arrays to an OpenAI-compatible (passthrough) provider", provider)
}

// toGemini translates a canonical OpenAI chat request into a Gemini request.
func toGemini(req *openai.ChatCompletionRequest) *generateRequest {
	out := &generateRequest{}

	cfg := &generationConfig{Temperature: req.Temperature, TopP: req.TopP}
	switch {
	case req.MaxTokens != nil:
		cfg.MaxOutputTokens = req.MaxTokens
	case req.MaxCompletionTokens != nil:
		cfg.MaxOutputTokens = req.MaxCompletionTokens
	}
	if req.Stop != nil {
		cfg.StopSequences = req.Stop.Values
	}
	applyResponseFormat(cfg, req.ResponseFormat)
	out.GenerationConfig = cfg

	// toolNames maps an assistant tool_call ID to its function name so that a
	// later "tool" role message that omits Name can recover it (OpenAI clients
	// often send only tool_call_id). Gemini requires a non-empty
	// functionResponse.name.
	toolNames := map[string]string{}
	for i := range req.Messages {
		if req.Messages[i].Role != "assistant" {
			continue
		}
		for _, tc := range req.Messages[i].ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}
	}

	out.Contents = make([]content, 0, len(req.Messages))
	var systemParts []string
	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system", "developer":
			if t := m.Content.String(); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			appendContent(out, "user", contentToParts(m.Content))
		case "assistant":
			parts := contentToParts(m.Content)
			for _, tc := range m.ToolCalls {
				parts = append(parts, part{FunctionCall: &fnCall{
					Name: tc.Function.Name, Args: rawOrEmptyObject(tc.Function.Arguments),
				}})
			}
			appendContent(out, "model", parts)
		case "tool":
			name := m.Name
			if name == "" {
				name = toolNames[m.ToolCallID]
			}
			appendContent(out, "user", []part{{FunctionResponse: &fnResponse{
				Name: name, Response: wrapToolResult(m.Content.String()),
			}}})
		}
	}
	if len(systemParts) > 0 {
		out.SystemInstruction = &content{Parts: []part{{Text: strings.Join(systemParts, "\n\n")}}}
	}

	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if len(out.Tools) == 0 {
			out.Tools = []geminiTool{{FunctionDeclarations: make([]fnDecl, 0, len(req.Tools))}}
		}
		out.Tools[0].FunctionDeclarations = append(out.Tools[0].FunctionDeclarations, fnDecl{
			Name: t.Function.Name, Description: t.Function.Description,
			Parameters: cleanSchema(t.Function.Parameters),
		})
	}

	return out
}

// applyResponseFormat maps an OpenAI response_format onto the Gemini
// generationConfig: json_object requests a JSON MIME type; json_schema also
// supplies the sanitized response schema (the OpenAI wrapper carries the schema
// under "schema"; "strict" is stripped by the sanitizer).
func applyResponseFormat(cfg *generationConfig, rf *openai.ResponseFormat) {
	if rf == nil {
		return
	}
	switch rf.Type {
	case "json_object":
		cfg.ResponseMimeType = "application/json"
	case "json_schema":
		cfg.ResponseMimeType = "application/json"
		if schema := extractJSONSchema(rf.JSONSchema); len(schema) > 0 {
			cfg.ResponseSchema = cleanSchema(schema)
		}
	}
}

// extractJSONSchema pulls the inner JSON Schema out of an OpenAI
// response_format.json_schema wrapper ({"name":...,"schema":{...},"strict":...}).
func extractJSONSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var wrapper struct {
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(raw, &wrapper) == nil && len(wrapper.Schema) > 0 {
		return wrapper.Schema
	}
	return nil
}

func appendContent(req *generateRequest, role string, parts []part) {
	if len(parts) == 0 {
		return
	}
	if n := len(req.Contents); n > 0 && req.Contents[n-1].Role == role {
		req.Contents[n-1].Parts = append(req.Contents[n-1].Parts, parts...)
		return
	}
	req.Contents = append(req.Contents, content{Role: role, Parts: parts})
}

func contentToParts(c openai.MessageContent) []part {
	if c.IsString {
		if c.Text == "" {
			return nil
		}
		return []part{{Text: c.Text}}
	}
	var parts []part
	for _, p := range c.Parts {
		switch p.Type {
		case "text":
			parts = append(parts, part{Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				if d := dataURIToInline(p.ImageURL.URL); d != nil {
					parts = append(parts, part{InlineData: d})
				}
			}
		}
	}
	return parts
}

// dataURIToInline parses a data: URI into Gemini inlineData. Plain http(s) URLs
// are not inlineable and are skipped (Gemini requires uploaded file refs).
func dataURIToInline(url string) *inlineData {
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	comma := strings.IndexByte(url, ',')
	if comma == -1 {
		return nil
	}
	meta := url[5:comma]
	mime := meta
	if semi := strings.IndexByte(meta, ';'); semi != -1 {
		mime = meta[:semi]
	}
	return &inlineData{MimeType: mime, Data: url[comma+1:]}
}

// wrapToolResult ensures the function response is a JSON object as Gemini
// requires. A bare string is wrapped as {"result": "..."}.
func wrapToolResult(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	b, _ := json.Marshal(map[string]string{"result": s})
	return b
}

// fromGemini translates a Gemini response into OpenAI shape.
func fromGemini(resp *generateResponse, model, id string, created int64) *openai.ChatCompletionResponse {
	msg := openai.Message{Role: "assistant"}
	finish := "stop"
	var text strings.Builder
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		for _, p := range cand.Content.Parts {
			if p.Text != "" {
				text.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
					ID: genID(), Type: "function",
					Function: openai.FunctionCall{Name: p.FunctionCall.Name, Arguments: string(rawOrEmptyObject(string(p.FunctionCall.Args)))},
				})
			}
		}
		finish = mapFinishReason(cand.FinishReason, len(msg.ToolCalls) > 0)
	}
	msg.Content = openai.Str(text.String())

	out := &openai.ChatCompletionResponse{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []openai.Choice{{Index: 0, Message: msg, FinishReason: finish}},
	}
	if resp.UsageMetadata != nil {
		out.Usage = &openai.Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
		}
	}
	return out
}

func mapFinishReason(r string, hasTools bool) string {
	if hasTools {
		return "tool_calls"
	}
	switch r {
	case "STOP", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT",
		"SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "LANGUAGE", "OTHER":
		// LiteLLM maps OTHER -> content_filter.
		return "content_filter"
	default:
		return "stop"
	}
}

func rawOrEmptyObject(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s != "" && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage(`{}`)
}

// emptyObjectSchema is the fallback parameters schema for tools that supply no
// (or invalid) JSON Schema.
const emptyObjectSchema = `{"type":"object","properties":{}}`

// cleanSchema sanitizes an OpenAI/JSON-Schema function-parameters document into
// the subset Gemini accepts. OpenAI tool schemas generated by pydantic/zod
// routinely include keys (additionalProperties, $schema, title, ...) and
// constructs (type arrays, item-less arrays) that make Gemini reject the
// request with HTTP 400. We parse the schema and recursively rewrite it.
//
// If the input is empty or not a JSON object we fall back to an empty-object
// schema rather than forwarding something Gemini will refuse.
func cleanSchema(s json.RawMessage) json.RawMessage {
	if len(s) == 0 {
		return json.RawMessage(emptyObjectSchema)
	}
	var m map[string]any
	if err := json.Unmarshal(s, &m); err != nil || m == nil {
		return json.RawMessage(emptyObjectSchema)
	}
	sanitizeSchema(m)
	out, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(emptyObjectSchema)
	}
	return out
}

// schemaDropKeys are JSON Schema keywords Gemini rejects outright.
var schemaDropKeys = []string{
	"additionalProperties", "$schema", "strict", "title",
	"default", "examples", "$id", "$comment",
}

// sanitizeSchema recursively rewrites a parsed JSON Schema object in place to
// keep only what Gemini accepts.
//
// TODO: inline $ref against $defs/definitions. For now $ref/$defs are left
// untouched (best effort) rather than dropped, so we don't crash on them.
func sanitizeSchema(m map[string]any) {
	for _, k := range schemaDropKeys {
		delete(m, k)
	}

	// Gemini only honours a couple of "format" values.
	if f, ok := m["format"].(string); ok && f != "enum" && f != "date-time" {
		delete(m, "format")
	}

	// type:["string","null"] -> type:"string" + nullable:true.
	if arr, ok := m["type"].([]any); ok {
		var nonNull string
		nullable := false
		for _, v := range arr {
			s, _ := v.(string)
			if s == "null" {
				nullable = true
				continue
			}
			if nonNull == "" {
				nonNull = s
			}
		}
		if nonNull != "" {
			m["type"] = nonNull
		} else {
			delete(m, "type")
		}
		if nullable {
			m["nullable"] = true
		}
	}

	// A type:"array" must carry items; default to an array of strings.
	if t, ok := m["type"].(string); ok && t == "array" {
		if _, ok := m["items"]; !ok {
			m["items"] = map[string]any{"type": "string"}
		}
	}

	// Recurse into nested schemas.
	if props, ok := m["properties"].(map[string]any); ok {
		for _, v := range props {
			if child, ok := v.(map[string]any); ok {
				sanitizeSchema(child)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		sanitizeSchema(items)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := m[key].([]any); ok {
			for _, v := range arr {
				if child, ok := v.(map[string]any); ok {
					sanitizeSchema(child)
				}
			}
		}
	}
}

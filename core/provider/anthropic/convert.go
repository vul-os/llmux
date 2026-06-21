package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/llmux/llmux/core/openai"
)

const defaultMaxTokens = 4096

// toAnthropic translates a canonical OpenAI chat request into an Anthropic
// Messages request.
func toAnthropic(req *openai.ChatCompletionRequest, target string) (*messagesRequest, error) {
	out := &messagesRequest{
		Model:       target,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}

	// max_tokens is required by Anthropic.
	switch {
	case req.MaxTokens != nil:
		out.MaxTokens = *req.MaxTokens
	case req.MaxCompletionTokens != nil:
		out.MaxTokens = *req.MaxCompletionTokens
	default:
		out.MaxTokens = defaultMaxTokens
	}

	if req.Stop != nil {
		out.StopSequences = req.Stop.Values
	}

	out.Messages = make([]message, 0, len(req.Messages))
	var systemParts []string
	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system", "developer":
			if t := m.Content.String(); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			appendMessage(out, "user", contentToBlocks(m.Content))
		case "assistant":
			blocks := contentToBlocks(m.Content)
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, block{
					Type: "tool_use", ID: tc.ID, Name: tc.Function.Name,
					Input: rawOrEmptyObject(tc.Function.Arguments),
				})
			}
			appendMessage(out, "assistant", blocks)
		case "tool":
			appendMessage(out, "user", []block{{
				Type: "tool_result", ToolUseID: m.ToolCallID,
				Content: jsonString(m.Content.String()),
			}})
		}
	}
	// Anthropic has no native response_format; emulate LiteLLM's universal
	// fallback by appending a system instruction asking for JSON output.
	if instr := responseFormatInstruction(req.ResponseFormat); instr != "" {
		systemParts = append(systemParts, instr)
	}
	out.System = strings.Join(systemParts, "\n\n")

	if len(req.Tools) > 0 {
		out.Tools = make([]tool, 0, len(req.Tools))
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, tool{
			Name: t.Function.Name, Description: t.Function.Description,
			InputSchema: rawOrEmptyObject(string(t.Function.Parameters)),
		})
	}
	out.ToolChoice = convertToolChoice(req.ToolChoice)
	out.ToolChoice = applyParallelToolCalls(out.ToolChoice, req.ParallelToolCalls)

	if req.User != "" {
		out.Metadata = &metadata{UserID: req.User}
	}

	return out, nil
}

// responseFormatInstruction returns a system-prompt addendum implementing the
// universal JSON-mode fallback for providers (Anthropic) without a native
// response_format. For json_schema it includes the schema so the model knows
// the target shape.
func responseFormatInstruction(rf *openai.ResponseFormat) string {
	if rf == nil {
		return ""
	}
	switch rf.Type {
	case "json_object":
		return "You must respond with valid JSON only."
	case "json_schema":
		instr := "You must respond with valid JSON only."
		if schema := jsonSchemaForInstruction(rf.JSONSchema); schema != "" {
			instr += " The JSON must conform to this JSON schema:\n" + schema
		}
		return instr
	}
	return ""
}

// jsonSchemaForInstruction extracts the inner schema from an OpenAI
// response_format.json_schema wrapper ({"name":...,"schema":{...},"strict":...}),
// falling back to the raw value if it has no "schema" key.
func jsonSchemaForInstruction(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var wrapper struct {
		Schema json.RawMessage `json:"schema"`
	}
	if json.Unmarshal(raw, &wrapper) == nil && len(wrapper.Schema) > 0 {
		return string(wrapper.Schema)
	}
	return string(raw)
}

// applyParallelToolCalls threads req.ParallelToolCalls into Anthropic's
// tool_choice as "disable_parallel_tool_use" (defaulting tool_choice to
// {"type":"auto"} when unset).
func applyParallelToolCalls(choice json.RawMessage, parallel *bool) json.RawMessage {
	if parallel == nil {
		return choice
	}
	m := map[string]any{}
	if len(choice) > 0 {
		if json.Unmarshal(choice, &m) != nil {
			m = map[string]any{}
		}
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "auto"
	}
	m["disable_parallel_tool_use"] = !*parallel
	b, err := json.Marshal(m)
	if err != nil {
		return choice
	}
	return b
}

// appendMessage appends blocks, merging into the previous message if it has the
// same role (Anthropic prefers alternating turns and merged tool results).
func appendMessage(req *messagesRequest, role string, blocks []block) {
	if len(blocks) == 0 {
		return
	}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == role {
		req.Messages[n-1].Content = append(req.Messages[n-1].Content, blocks...)
		return
	}
	req.Messages = append(req.Messages, message{Role: role, Content: blocks})
}

// contentToBlocks converts message content into Anthropic blocks.
func contentToBlocks(c openai.MessageContent) []block {
	if c.IsString {
		if c.Text == "" {
			return nil
		}
		return []block{{Type: "text", Text: c.Text}}
	}
	var blocks []block
	for _, p := range c.Parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, block{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL != nil {
				blocks = append(blocks, imageBlock(p.ImageURL.URL))
			}
		}
	}
	return blocks
}

// imageBlock builds an image block from a data: URI or a plain URL.
func imageBlock(url string) block {
	if strings.HasPrefix(url, "data:") {
		// data:<media-type>;base64,<data>
		if comma := strings.IndexByte(url, ','); comma != -1 {
			meta := url[5:comma]
			data := url[comma+1:]
			mediaType := meta
			if semi := strings.IndexByte(meta, ';'); semi != -1 {
				mediaType = meta[:semi]
			}
			return block{Type: "image", Source: &imageSource{
				Type: "base64", MediaType: mediaType, Data: data,
			}}
		}
	}
	return block{Type: "image", Source: &imageSource{Type: "url", URL: url}}
}

func convertToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required":
			return json.RawMessage(`{"type":"any"}`)
		case "none":
			return json.RawMessage(`{"type":"none"}`)
		}
		return nil
	}
	// {"type":"function","function":{"name":"x"}} -> {"type":"tool","name":"x"}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		b, _ := json.Marshal(map[string]string{"type": "tool", "name": obj.Function.Name})
		return b
	}
	return nil
}

// fromAnthropic translates a non-streaming Anthropic response into OpenAI shape.
func fromAnthropic(resp *messagesResponse, requestedModel string) *openai.ChatCompletionResponse {
	msg := openai.Message{Role: "assistant"}
	var textParts []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID: b.ID, Type: "function",
				Function: openai.FunctionCall{Name: b.Name, Arguments: string(b.Input)},
			})
		}
	}
	msg.Content = openai.Str(strings.Join(textParts, ""))

	model := resp.Model
	if model == "" {
		model = requestedModel
	}
	return &openai.ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Model:   model,
		Choices: []openai.Choice{{Index: 0, Message: msg, FinishReason: mapStopReason(resp.StopReason)}},
		Usage: &openai.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

// mapStopReason maps Anthropic stop reasons to OpenAI finish reasons.
func mapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		return "stop"
	}
}

// rawOrEmptyObject returns raw JSON or "{}" if empty/invalid.
func rawOrEmptyObject(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage(`{}`)
}

// jsonString JSON-encodes a string for use as tool_result content.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

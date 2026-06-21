package bedrock

import (
	"encoding/json"
	"strings"

	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

const defaultMaxTokens = 4096

// toInvoke translates a canonical OpenAI chat request into a Bedrock-flavoured
// Anthropic Messages body. The model id is intentionally NOT included: Bedrock
// reads it from the request URL.
//
// This mirrors the anthropic package's toAnthropic logic, narrowed to the
// fields Bedrock documents. UNVERIFIED against the live service.
func toInvoke(req *openai.ChatCompletionRequest) *invokeRequest {
	out := &invokeRequest{
		AnthropicVersion: bedrockAnthropicVersion,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
	}
	out.Messages = make([]message, 0, len(req.Messages))
	out.Tools = make([]tool, 0, len(req.Tools))

	// max_tokens is required.
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
	out.System = strings.Join(systemParts, "\n\n")

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

	return out
}

// appendMessage appends blocks, merging into the previous message if it has the
// same role (Anthropic prefers alternating turns and merged tool results).
func appendMessage(req *invokeRequest, role string, blocks []block) {
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
	blocks := make([]block, 0, len(c.Parts))
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

// imageBlock builds an image block from a data: URI or a plain URL. A data: URI
// is only labeled as a base64 source when it actually carries the ";base64"
// marker; a raw (non-base64) data payload keeps a non-base64 source type.
func imageBlock(url string) block {
	if mediaType, data, isBase64, ok := provider.ParseDataURI(url); ok {
		srcType := "base64"
		if !isBase64 {
			srcType = "text"
		}
		return block{Type: "image", Source: &imageSource{
			Type: srcType, MediaType: mediaType, Data: data,
		}}
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
		default:
			return nil
		}
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

// fromInvoke translates a non-streaming Bedrock/Anthropic response into the
// canonical OpenAI shape. UNVERIFIED against the live service.
func fromInvoke(resp *invokeResponse, requestedModel string) *openai.ChatCompletionResponse {
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

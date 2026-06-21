package cohere

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
				return nil, errTokenArrayInput("Cohere")
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

// toCohere translates a canonical OpenAI chat request into a Cohere v2 chat
// request.
func toCohere(req *openai.ChatCompletionRequest, target string) (*chatRequest, error) {
	out := &chatRequest{
		Model:       target,
		Temperature: req.Temperature,
		P:           req.TopP,
		Stream:      req.Stream,
	}
	out.Messages = make([]message, 0, len(req.Messages))
	out.Tools = make([]tool, 0, len(req.Tools))

	switch {
	case req.MaxTokens != nil:
		out.MaxTokens = req.MaxTokens
	case req.MaxCompletionTokens != nil:
		out.MaxTokens = req.MaxCompletionTokens
	}

	if req.Stop != nil {
		out.StopSequences = req.Stop.Values
	}

	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case "system", "developer":
			out.Messages = append(out.Messages, message{
				Role: "system", Content: textPayload(m.Content),
			})
		case "user":
			out.Messages = append(out.Messages, message{
				Role: "user", Content: textPayload(m.Content),
			})
		case "assistant":
			msg := message{Role: "assistant"}
			if c := textPayload(m.Content); c != nil {
				msg.Content = c
			}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, toolCall{
					ID: tc.ID, Type: "function",
					Function: functionCall{
						Name:      tc.Function.Name,
						Arguments: argsOrEmptyObject(tc.Function.Arguments),
					},
				})
			}
			out.Messages = append(out.Messages, msg)
		case "tool":
			out.Messages = append(out.Messages, message{
				Role: "tool", ToolCallID: m.ToolCallID,
				Content: textPayload(m.Content),
			})
		}
	}

	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out.Tools = append(out.Tools, tool{
			Type: "function",
			Function: functionDef{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  rawOrEmptyObject(string(t.Function.Parameters)),
			},
		})
	}

	return out, nil
}

// textPayload renders message content as the Cohere `content` field: a JSON
// string for simple text, or a [{type:"text",text}] array for multipart text.
// It returns nil when there is no text content.
func textPayload(c openai.MessageContent) json.RawMessage {
	if c.IsString {
		if c.Text == "" {
			return nil
		}
		return jsonString(c.Text)
	}
	parts := make([]textContent, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Type == "text" {
			parts = append(parts, textContent{Type: "text", Text: p.Text})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	b, _ := json.Marshal(parts)
	return b
}

// fromCohere translates a non-streaming Cohere response into OpenAI shape.
func fromCohere(resp *chatResponse, requestedModel string) *openai.ChatCompletionResponse {
	msg := openai.Message{Role: "assistant"}
	var textParts []string
	for _, c := range resp.Message.Content {
		if c.Type == "" || c.Type == "text" {
			textParts = append(textParts, c.Text)
		}
	}
	msg.Content = openai.Str(strings.Join(textParts, ""))

	for _, tc := range resp.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
			ID: tc.ID, Type: "function",
			Function: openai.FunctionCall{
				Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			},
		})
	}

	finish := mapFinishReason(resp.FinishReason)
	if len(msg.ToolCalls) > 0 {
		finish = "tool_calls"
	}

	in := resp.Usage.Tokens.InputTokens
	out := resp.Usage.Tokens.OutputTokens
	return &openai.ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Model:   requestedModel,
		Choices: []openai.Choice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage: &openai.Usage{
			PromptTokens:     in,
			CompletionTokens: out,
			TotalTokens:      in + out,
		},
	}
}

// mapFinishReason maps Cohere finish reasons to OpenAI finish reasons.
func mapFinishReason(r string) string {
	switch r {
	case "COMPLETE", "STOP", "STOP_SEQUENCE":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "TOOL_CALL", "TOOL_CALLS":
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

// argsOrEmptyObject returns a valid JSON arguments string, defaulting to "{}".
func argsOrEmptyObject(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || !json.Valid([]byte(s)) {
		return "{}"
	}
	return s
}

// jsonString JSON-encodes a string for use as a content field.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

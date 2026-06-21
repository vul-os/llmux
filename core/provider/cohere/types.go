package cohere

import "encoding/json"

// Cohere v2 Chat API wire types (the subset llmux translates to/from).
//
// These mappings were written to the documented Cohere v2 /chat API and should
// be verified against live responses, as upstream shapes may have drifted.

type chatRequest struct {
	Model         string    `json:"model"`
	Messages      []message `json:"messages"`
	Tools         []tool    `json:"tools,omitempty"`
	Temperature   *float64  `json:"temperature,omitempty"`
	MaxTokens     *int      `json:"max_tokens,omitempty"`
	P             *float64  `json:"p,omitempty"`
	StopSequences []string  `json:"stop_sequences,omitempty"`
	Stream        bool      `json:"stream,omitempty"`
}

// message is one chat turn. Content is a string for user/system/tool turns;
// assistant turns may instead carry tool_calls.
type message struct {
	Role       string          `json:"role"` // system | user | assistant | tool
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// toolCall is an assistant-issued function call.
type toolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // "function"
	Function functionCall `json:"function"`
}

// functionCall carries the name and JSON-encoded arguments string.
type functionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// tool advertises a callable function to the model.
type tool struct {
	Type     string      `json:"type"` // "function"
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// textContent is one element of an array-form content field.
type textContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// Non-streaming response.
type chatResponse struct {
	ID           string          `json:"id"`
	FinishReason string          `json:"finish_reason"`
	Message      responseMessage `json:"message"`
	Usage        usage           `json:"usage"`
}

type responseMessage struct {
	Role      string        `json:"role"` // "assistant"
	Content   []textContent `json:"content"`
	ToolCalls []toolCall    `json:"tool_calls"`
}

type usage struct {
	Tokens tokens `json:"tokens"`
}

type tokens struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Streaming event envelope. Cohere v2 emits events discriminated by "type".
type streamEvent struct {
	Type  string       `json:"type"`
	Index int          `json:"index"`
	Delta *streamDelta `json:"delta,omitempty"`
}

type streamDelta struct {
	Message      *streamMessage `json:"message,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
	Usage        *usage         `json:"usage,omitempty"`
}

type streamMessage struct {
	Content   *streamContent  `json:"content,omitempty"`
	ToolCalls *streamToolCall `json:"tool_calls,omitempty"`
}

type streamContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type streamToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type errorEnvelope struct {
	Message string `json:"message"`
}

// Embeddings wire types (Cohere v2 /embed).

type embedRequest struct {
	Model          string   `json:"model"`
	Texts          []string `json:"texts"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
}

type embedResponse struct {
	Embeddings embeddingsByType `json:"embeddings"`
}

type embeddingsByType struct {
	Float [][]float64 `json:"float"`
}

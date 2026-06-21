package anthropic

import "encoding/json"

// Anthropic Messages API wire types (the subset llmux translates to/from).

type messagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []message       `json:"messages"`
	System        string          `json:"system,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []tool          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Metadata      *metadata       `json:"metadata,omitempty"`
}

type metadata struct {
	UserID string `json:"user_id,omitempty"`
}

type message struct {
	Role    string  `json:"role"` // user | assistant
	Content []block `json:"content"`
}

// block is a content block. Fields are a union across block types.
type block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *imageSource `json:"source,omitempty"`

	// tool_use (assistant)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result (user)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Non-streaming response.
type messagesResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Role       string  `json:"role"`
	Model      string  `json:"model"`
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      usage   `json:"usage"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Streaming event envelope.
type streamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message,omitempty"`
	ContentBlock *block          `json:"content_block,omitempty"`
	Delta        *streamDelta    `json:"delta,omitempty"`
	Usage        *usage          `json:"usage,omitempty"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type errorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

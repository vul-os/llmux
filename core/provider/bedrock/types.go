package bedrock

import "encoding/json"

// Bedrock invokes Anthropic Claude models with the Anthropic Messages schema,
// with two differences from the direct Anthropic API:
//   - the model id is carried in the URL path, not the body;
//   - the body must set "anthropic_version":"bedrock-2023-05-31" instead of
//     sending an api-version header.
//
// These wire types are a focused subset of the Anthropic Messages schema as
// documented for Bedrock. They are reimplemented here (rather than imported)
// because the anthropic package's equivalents are unexported.
//
// IMPORTANT: the request/response mapping below is written to the *documented*
// Bedrock+Anthropic schema and is UNVERIFIED against the live service.

const bedrockAnthropicVersion = "bedrock-2023-05-31"

// invokeRequest is the JSON body POSTed to /model/{id}/invoke. Note there is no
// "model" field: Bedrock takes the model id from the URL.
type invokeRequest struct {
	AnthropicVersion string          `json:"anthropic_version"`
	MaxTokens        int             `json:"max_tokens"`
	Messages         []message       `json:"messages"`
	System           string          `json:"system,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	StopSequences    []string        `json:"stop_sequences,omitempty"`
	Tools            []tool          `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
}

type message struct {
	Role    string  `json:"role"` // user | assistant
	Content []block `json:"content"`
}

// block is a content block; fields form a union across block types.
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

// invokeResponse is the Anthropic-shaped body returned by /invoke.
type invokeResponse struct {
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

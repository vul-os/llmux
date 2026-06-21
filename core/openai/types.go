// Package openai defines the canonical wire types for llmux.
//
// The OpenAI HTTP schema IS llmux's internal contract: clients in every
// language speak it via their existing OpenAI SDK, and every provider is an
// adapter behind it. These types are the single source of truth — nothing
// provider-specific is allowed to leak through them.
package openai

import (
	"bytes"
	"encoding/json"
)

// ---------------------------------------------------------------------------
// Chat completion request
// ---------------------------------------------------------------------------

// ChatCompletionRequest mirrors POST /v1/chat/completions. Optional scalars are
// pointers so we can distinguish "unset" from a zero value when forwarding.
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	N                   *int               `json:"n,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *StreamOptions     `json:"stream_options,omitempty"`
	Stop                *StringOrArray     `json:"stop,omitempty"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	PresencePenalty     *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64           `json:"frequency_penalty,omitempty"`
	LogitBias           map[string]float64 `json:"logit_bias,omitempty"`
	Logprobs            *bool              `json:"logprobs,omitempty"`
	TopLogprobs         *int               `json:"top_logprobs,omitempty"`
	User                string             `json:"user,omitempty"`
	Seed                *int               `json:"seed,omitempty"`

	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"` // "none"|"auto"|"required"|{object}
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`

	// Metadata is an OpenAI-standard field every SDK can send. llmux reads
	// routing/budget hints from here so no custom client is ever required.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// StreamOptions controls streaming behaviour.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ResponseFormat selects text / json_object / json_schema output.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// ---------------------------------------------------------------------------
// Messages & content
// ---------------------------------------------------------------------------

// Message is one chat message. Content is polymorphic: a plain string or an
// array of typed parts (text, image, audio).
type Message struct {
	Role       string         `json:"role"`
	Content    MessageContent `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Refusal    string         `json:"refusal,omitempty"`
}

// MarshalJSON omits the content field entirely when it was never set (zero
// value), rather than emitting "content":null. (struct-typed omitempty is a
// no-op, so this is done manually.) An explicit null from the client, a string,
// or a parts array are all preserved.
func (m Message) MarshalJSON() ([]byte, error) {
	type alias struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content,omitempty"`
		Name       string          `json:"name,omitempty"`
		ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		Refusal    string          `json:"refusal,omitempty"`
	}
	a := alias{Role: m.Role, Name: m.Name, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID, Refusal: m.Refusal}
	if m.Content.set() {
		cb, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		a.Content = cb
	}
	return json.Marshal(a)
}

// MessageContent holds either a string or a slice of ContentPart. Exactly one
// of Text / Parts is populated; IsString records which form the client sent so
// we round-trip identically.
type MessageContent struct {
	Text     string
	Parts    []ContentPart
	IsString bool
	isNull   bool
}

// Str builds string content.
func Str(s string) MessageContent { return MessageContent{Text: s, IsString: true} }

// set reports whether content was populated (string, parts, or explicit null),
// as opposed to a never-set zero value.
func (c MessageContent) set() bool { return c.IsString || c.Parts != nil || c.isNull }

// String returns the flattened text of the content (concatenating text parts).
func (c MessageContent) String() string {
	if c.IsString {
		return c.Text
	}
	var b bytes.Buffer
	for _, p := range c.Parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.isNull {
		return []byte("null"), nil
	}
	if c.IsString {
		return json.Marshal(c.Text)
	}
	if c.Parts == nil {
		return []byte("null"), nil
	}
	return json.Marshal(c.Parts)
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		c.isNull = true
		return nil
	}
	if data[0] == '"' {
		c.IsString = true
		return json.Unmarshal(data, &c.Text)
	}
	return json.Unmarshal(data, &c.Parts)
}

// ContentPart is one element of multimodal content.
type ContentPart struct {
	Type       string      `json:"type"` // text | image_url | input_audio
	Text       string      `json:"text,omitempty"`
	ImageURL   *ImageURL   `json:"image_url,omitempty"`
	InputAudio *InputAudio `json:"input_audio,omitempty"`
}

// ImageURL is a URL or data: URI plus an optional detail hint.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// InputAudio is base64 audio with a format.
type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// ---------------------------------------------------------------------------
// Tools / function calling
// ---------------------------------------------------------------------------

// Tool is a callable function advertised to the model.
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a function tool. Parameters is a raw JSON Schema.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolCall is a model-emitted call to a tool.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`  // "function"
	Index    *int         `json:"index,omitempty"` // present in streaming deltas
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the name and JSON-encoded arguments.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ---------------------------------------------------------------------------
// Chat completion response (non-streaming)
// ---------------------------------------------------------------------------

// ChatCompletionResponse is the non-streaming response object.
type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"` // "chat.completion"
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
}

// Choice is one completion alternative.
type Choice struct {
	Index        int       `json:"index"`
	Message      Message   `json:"message"`
	FinishReason string    `json:"finish_reason,omitempty"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
}

// Logprobs is an opaque passthrough of token logprob data.
type Logprobs struct {
	Content json.RawMessage `json:"content,omitempty"`
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// ChatCompletionChunk is one SSE chunk (object="chat.completion.chunk").
type ChatCompletionChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
	Choices           []ChunkChoice `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is a streaming choice carrying a delta.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental message content in a stream chunk.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Refusal   string     `json:"refusal,omitempty"`
}

// ---------------------------------------------------------------------------
// Usage & cost
// ---------------------------------------------------------------------------

// Usage reports token counts. Cost is an llmux extension populated by the
// pricing layer — additive, so OpenAI clients ignore it harmlessly.
type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	Cost                *Cost                `json:"cost,omitempty"`
}

// PromptTokensDetails breaks down prompt tokens (OpenAI-standard). CachedTokens
// are billed at the provider's discounted cache-read rate.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
	AudioTokens  int `json:"audio_tokens,omitempty"`
}

// Cost is the computed dollar cost of a request (llmux extension).
type Cost struct {
	InputCost  float64 `json:"input_cost"`
	OutputCost float64 `json:"output_cost"`
	TotalCost  float64 `json:"total_cost"`
	Currency   string  `json:"currency"` // "USD"
}

// ---------------------------------------------------------------------------
// Embeddings
// ---------------------------------------------------------------------------

// EmbeddingRequest mirrors POST /v1/embeddings. Input is string|[]string|tokens.
type EmbeddingRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	User           string          `json:"user,omitempty"`
}

// EmbeddingResponse is the embeddings result object.
type EmbeddingResponse struct {
	Object string          `json:"object"` // "list"
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  *Usage          `json:"usage,omitempty"`
}

// EmbeddingData is one embedding vector.
type EmbeddingData struct {
	Object    string    `json:"object"` // "embedding"
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// ---------------------------------------------------------------------------
// Models listing
// ---------------------------------------------------------------------------

// ModelList is the GET /v1/models response.
type ModelList struct {
	Object string  `json:"object"` // "list"
	Data   []Model `json:"data"`
}

// Model is one entry in the models list. Pricing/context fields are llmux
// extensions surfaced from the catalog.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// llmux extensions
	Provider      string   `json:"provider,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxOutput     int      `json:"max_output_tokens,omitempty"`
	InputPrice    float64  `json:"input_price_per_mtok,omitempty"`
	OutputPrice   float64  `json:"output_price_per_mtok,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrorResponse is the OpenAI-shaped error envelope.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// APIError is the error body.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// NewError builds an ErrorResponse.
func NewError(msg, typ, code string) *ErrorResponse {
	return &ErrorResponse{Error: APIError{Message: msg, Type: typ, Code: code}}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// StringOrArray holds a JSON value that may be a string or []string (e.g. stop).
type StringOrArray struct {
	Values []string
	single bool
}

func (s StringOrArray) MarshalJSON() ([]byte, error) {
	if s.single && len(s.Values) == 1 {
		return json.Marshal(s.Values[0])
	}
	return json.Marshal(s.Values)
}

func (s *StringOrArray) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		s.Values = []string{v}
		s.single = true
		return nil
	}
	return json.Unmarshal(data, &s.Values)
}

package gemini

import "encoding/json"

// Google Gemini generateContent API wire types (the subset llmux translates).

type generateRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
}

type generationConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"` // "user" | "model"
	Parts []part `json:"parts"`
}

type part struct {
	Text             string      `json:"text,omitempty"`
	InlineData       *inlineData `json:"inlineData,omitempty"`
	FunctionCall     *fnCall     `json:"functionCall,omitempty"`
	FunctionResponse *fnResponse `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type fnCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type fnResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []fnDecl `json:"functionDeclarations,omitempty"`
}

type fnDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Response (both unary and per-chunk streaming share this shape).
type generateResponse struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
}

type candidate struct {
	Content      content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
	Index        int     `json:"index"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// Embeddings wire types.

type embedContentPayload struct {
	Parts []part `json:"parts"`
}

type embedContentRequest struct {
	Content embedContentPayload `json:"content"`
}

type embedding struct {
	Values []float64 `json:"values"`
}

type embedContentResponse struct {
	Embedding embedding `json:"embedding"`
}

type batchEmbedItem struct {
	Model   string              `json:"model"`
	Content embedContentPayload `json:"content"`
}

type batchEmbedRequest struct {
	Requests []batchEmbedItem `json:"requests"`
}

type batchEmbedResponse struct {
	Embeddings []embedding `json:"embeddings"`
}

type errorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

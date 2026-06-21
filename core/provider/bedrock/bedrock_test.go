package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// syntheticResponse is an Anthropic-shaped /invoke body the mock returns.
const syntheticResponse = `{
  "id": "msg_123",
  "type": "message",
  "role": "assistant",
  "model": "anthropic.claude-3-sonnet-20240229-v1:0",
  "content": [
    {"type": "text", "text": "Hello there"},
    {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "SF"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 11, "output_tokens": 7}
}`

func setFakeAWSEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "")
}

// newMock returns a Provider pointed at an httptest server that asserts the
// request path/body and returns the synthetic Anthropic-shaped response. The
// captured request body is delivered on bodyCh.
func newMock(t *testing.T, modelID string) (*Provider, *httptest.Server, chan []byte) {
	t.Helper()
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/model/" + modelID + "/invoke"
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing SigV4 Authorization header")
		}
		if !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("Authorization not SigV4: %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, syntheticResponse)
	}))
	t.Cleanup(srv.Close)

	p := New(config.ProviderConfig{
		Name:    "bedrock",
		BaseURL: srv.URL,
		Headers: map[string]string{"region": "us-east-1"},
	})
	return p, srv, bodyCh
}

func sampleRequest() *openai.ChatCompletionRequest {
	return &openai.ChatCompletionRequest{
		Model: "claude-sonnet",
		Messages: []openai.Message{
			{Role: "system", Content: openai.Str("be terse")},
			{Role: "user", Content: openai.Str("hi")},
		},
	}
}

func TestChatCompletion(t *testing.T) {
	setFakeAWSEnv(t)
	const modelID = "anthropic.claude-3-sonnet-20240229-v1:0"
	p, _, bodyCh := newMock(t, modelID)

	resp, err := p.ChatCompletion(context.Background(), sampleRequest(), modelID, nil)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Assert request body shape.
	reqBody := <-bodyCh
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &sent); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if v, _ := sent["anthropic_version"]; string(v) != `"`+bedrockAnthropicVersion+`"` {
		t.Errorf("anthropic_version = %s, want %q", v, bedrockAnthropicVersion)
	}
	if _, ok := sent["messages"]; !ok {
		t.Errorf("body missing messages")
	}
	if v, ok := sent["system"]; !ok || string(v) != `"be terse"` {
		t.Errorf("system = %s, want %q", v, "be terse")
	}
	if _, ok := sent["model"]; ok {
		t.Errorf("body must NOT contain model (it goes in the URL)")
	}

	// Assert translated response.
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	ch := resp.Choices[0]
	if got := ch.Message.Content.String(); got != "Hello there" {
		t.Errorf("content = %q, want %q", got, "Hello there")
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(ch.Message.ToolCalls))
	}
	tc := ch.Message.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	if !strings.Contains(tc.Function.Arguments, `"city"`) {
		t.Errorf("tool args = %q", tc.Function.Arguments)
	}
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", ch.FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 || resp.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestChatCompletionStream(t *testing.T) {
	setFakeAWSEnv(t)
	const modelID = "anthropic.claude-3-sonnet-20240229-v1:0"
	p, _, _ := newMock(t, modelID)

	includeUsage := true
	req := sampleRequest()
	req.StreamOptions = &openai.StreamOptions{IncludeUsage: includeUsage}

	var (
		sawRole    bool
		content    string
		sawTool    bool
		finish     string
		usageSeen  *openai.Usage
		chunkCount int
	)
	err := p.ChatCompletionStream(context.Background(), req, modelID, nil, func(c *openai.ChatCompletionChunk) error {
		chunkCount++
		if c.Usage != nil {
			usageSeen = c.Usage
		}
		for _, ch := range c.Choices {
			if ch.Delta.Role != "" {
				sawRole = true
			}
			content += ch.Delta.Content
			if len(ch.Delta.ToolCalls) > 0 {
				sawTool = true
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}

	if !sawRole {
		t.Errorf("no role delta chunk emitted")
	}
	if content != "Hello there" {
		t.Errorf("streamed content = %q, want %q", content, "Hello there")
	}
	if !sawTool {
		t.Errorf("no tool_call delta emitted")
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
	if usageSeen == nil || usageSeen.TotalTokens != 18 {
		t.Errorf("usage chunk = %+v", usageSeen)
	}
	if chunkCount < 4 {
		t.Errorf("chunk count = %d, want >= 4 (role, content, tool, finish)", chunkCount)
	}
}

// TestInvokeURLEncodesARN verifies an ARN-style target is fully percent-encoded
// by invokeURL: ":" becomes "%3A" and the internal "/" becomes "%2F" (the model
// id is a single path segment). Under-encoding here makes Bedrock's router 400.
func TestInvokeURLEncodesARN(t *testing.T) {
	p := New(config.ProviderConfig{
		Name:    "bedrock",
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		Headers: map[string]string{"region": "us-east-1"},
	})
	const arn = "arn:aws:bedrock:us-east-1:123:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0"
	got := p.invokeURL(arn)
	const want = "https://bedrock-runtime.us-east-1.amazonaws.com/model/" +
		"arn%3Aaws%3Abedrock%3Aus-east-1%3A123%3Ainference-profile%2Fus.anthropic.claude-3-5-sonnet-20241022-v2%3A0" +
		"/invoke"
	if got != want {
		t.Fatalf("invokeURL(arn)\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, ":0/invoke") || strings.Contains(got, "profile/us.anthropic") {
		t.Errorf("ARN left under-encoded: %s", got)
	}

	// The signer canonicalizes from URL.EscapedPath(); the request built from
	// this URL must round-trip the encoded path so the SigV4 canonical path
	// matches what is actually sent on the wire.
	req, err := http.NewRequest(http.MethodPost, got, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	wantPath := "/model/" +
		"arn%3Aaws%3Abedrock%3Aus-east-1%3A123%3Ainference-profile%2Fus.anthropic.claude-3-5-sonnet-20241022-v2%3A0" +
		"/invoke"
	if ep := req.URL.EscapedPath(); ep != wantPath {
		t.Fatalf("EscapedPath round-trip\n got: %s\nwant: %s", ep, wantPath)
	}
}

// TestChatCompletionARNTarget exercises a full ChatCompletion against the mock
// using an inference-profile ARN as the target. It asserts the request reaches
// the server with the fully percent-encoded path AND a valid SigV4 signature
// (i.e. the signed canonical path matches the encoded path on the wire).
func TestChatCompletionARNTarget(t *testing.T) {
	setFakeAWSEnv(t)
	const arn = "arn:aws:bedrock:us-east-1:123:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0"
	const wantEscaped = "/model/" +
		"arn%3Aaws%3Abedrock%3Aus-east-1%3A123%3Ainference-profile%2Fus.anthropic.claude-3-5-sonnet-20241022-v2%3A0" +
		"/invoke"

	pathCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathCh <- r.URL.EscapedPath()
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "AWS4-HMAC-SHA256") || !strings.Contains(auth, "Signature=") {
			t.Errorf("missing/invalid SigV4 Authorization: %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, syntheticResponse)
	}))
	t.Cleanup(srv.Close)

	p := New(config.ProviderConfig{
		Name:    "bedrock",
		BaseURL: srv.URL,
		Headers: map[string]string{"region": "us-east-1"},
	})

	resp, err := p.ChatCompletion(context.Background(), sampleRequest(), arn, nil)
	if err != nil {
		t.Fatalf("ChatCompletion(arn): %v", err)
	}
	if got := <-pathCh; got != wantEscaped {
		t.Fatalf("server saw escaped path\n got: %s\nwant: %s", got, wantEscaped)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
}

// TestNonAnthropicModelRejected verifies a non-Anthropic model id is hard-gated
// with a 400 provider error and does NOT hit the network (the mock server fails
// the test if it is ever called).
func TestNonAnthropicModelRejected(t *testing.T) {
	setFakeAWSEnv(t)
	const modelID = "amazon.nova-pro-v1:0"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("network was hit for non-anthropic model: %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	p := New(config.ProviderConfig{
		Name:    "bedrock",
		BaseURL: srv.URL,
		Headers: map[string]string{"region": "us-east-1"},
	})

	assert400 := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		pe, ok := err.(*provider.Error)
		if !ok {
			t.Fatalf("error type = %T, want *provider.Error", err)
		}
		if pe.StatusCode != http.StatusBadRequest {
			t.Errorf("StatusCode = %d, want 400", pe.StatusCode)
		}
		if !strings.Contains(strings.ToLower(pe.Error()), "anthropic") {
			t.Errorf("error message lacks guidance: %q", pe.Error())
		}
	}

	_, err := p.ChatCompletion(context.Background(), sampleRequest(), modelID, nil)
	assert400(err)

	err = p.ChatCompletionStream(context.Background(), sampleRequest(), modelID, nil,
		func(*openai.ChatCompletionChunk) error {
			t.Error("yield called for rejected model")
			return nil
		})
	assert400(err)
}

func TestEmbeddingsNotImplemented(t *testing.T) {
	setFakeAWSEnv(t)
	p := New(config.ProviderConfig{Name: "bedrock", Headers: map[string]string{"region": "us-east-1"}})
	_, err := p.Embeddings(context.Background(), &openai.EmbeddingRequest{}, "x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDefaultBaseURL(t *testing.T) {
	p := New(config.ProviderConfig{Name: "bedrock", Headers: map[string]string{"region": "eu-west-1"}})
	if p.baseURL != "https://bedrock-runtime.eu-west-1.amazonaws.com" {
		t.Errorf("baseURL = %q", p.baseURL)
	}
	if p.region != "eu-west-1" {
		t.Errorf("region = %q", p.region)
	}
}

func makeResp(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
	}
}

func newErrProvider() *Provider {
	return New(config.ProviderConfig{Name: "bedrock"})
}

func TestBedrockErrorCapitalMessage(t *testing.T) {
	p := newErrProvider()
	// AWS throttling bodies use a capital "Message" key.
	resp := makeResp(429, `{"Message":"Rate exceeded"}`, nil)
	e := p.errorFromResponse(context.Background(), resp)
	if e.Body.Error.Message != "Rate exceeded" {
		t.Errorf("message=%q want Rate exceeded", e.Body.Error.Message)
	}
	if e.Body.Error.Type != "rate_limit_error" {
		t.Errorf("type=%q want rate_limit_error", e.Body.Error.Type)
	}
}

func TestBedrockErrorLowerMessage(t *testing.T) {
	p := newErrProvider()
	resp := makeResp(400, `{"message":"bad input"}`, nil)
	e := p.errorFromResponse(context.Background(), resp)
	if e.Body.Error.Message != "bad input" {
		t.Errorf("message=%q", e.Body.Error.Message)
	}
	if e.Body.Error.Type != "invalid_request_error" {
		t.Errorf("type=%q", e.Body.Error.Type)
	}
}

func TestBedrockErrorAnthropicEnvelope(t *testing.T) {
	p := newErrProvider()
	resp := makeResp(400, `{"type":"error","error":{"type":"invalid_request_error","message":"nested"}}`, nil)
	e := p.errorFromResponse(context.Background(), resp)
	if e.Body.Error.Message != "nested" {
		t.Errorf("message=%q want nested", e.Body.Error.Message)
	}
}

func TestBedrockErrorXAmznErrorTypeHeader(t *testing.T) {
	p := newErrProvider()
	// Empty body but the error kind is only in the header.
	h := http.Header{}
	h.Set("X-Amzn-ErrorType", "ThrottlingException:http://internal.amazon.com/...")
	resp := makeResp(429, ``, h)
	e := p.errorFromResponse(context.Background(), resp)
	if e.Body.Error.Message != "ThrottlingException" {
		t.Errorf("message=%q want ThrottlingException (from header)", e.Body.Error.Message)
	}
	if e.Body.Error.Type != "rate_limit_error" {
		t.Errorf("type=%q want rate_limit_error", e.Body.Error.Type)
	}
}

func TestBedrockErrorTypeNormalizationTable(t *testing.T) {
	p := newErrProvider()
	cases := []struct {
		status   int
		wantType string
	}{
		{400, "invalid_request_error"},
		{401, "authentication_error"},
		{403, "permission_error"},
		{404, "not_found_error"},
		{429, "rate_limit_error"},
		{500, "api_error"},
	}
	for _, tc := range cases {
		resp := makeResp(tc.status, `{"message":"x"}`, nil)
		e := p.errorFromResponse(context.Background(), resp)
		if e.Body.Error.Type != tc.wantType {
			t.Errorf("status %d: type=%q want %q", tc.status, e.Body.Error.Type, tc.wantType)
		}
	}
}

func TestBedrockErrorContextLengthCode(t *testing.T) {
	p := newErrProvider()
	resp := makeResp(400, `{"message":"input exceeds the maximum context window"}`, nil)
	e := p.errorFromResponse(context.Background(), resp)
	if e.Body.Error.Code != "context_length_exceeded" {
		t.Errorf("code=%q want context_length_exceeded", e.Body.Error.Code)
	}
}

func TestBedrockErrorCapturesRateLimitHeaders(t *testing.T) {
	p := newErrProvider()
	ctx, sink := provider.WithHeaderSink(context.Background())
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	resp := makeResp(429, `{"Message":"slow"}`, h)
	_ = p.errorFromResponse(ctx, resp)
	if got := sink.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("captured x-ratelimit-remaining=%q want 0", got)
	}
}

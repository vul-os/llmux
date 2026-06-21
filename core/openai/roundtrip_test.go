package openai

import (
	"bytes"
	"encoding/json"
	"testing"
)

// remarshal unmarshals src into a fresh value of the same type as dst, then
// marshals it back out. It returns the re-marshaled bytes.
func remarshalReq(t *testing.T, src string) ([]byte, ChatCompletionRequest) {
	t.Helper()
	var v ChatCompletionRequest
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("unmarshal: %v\nsrc=%s", err, src)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out, v
}

func remarshalResp(t *testing.T, src string) ([]byte, ChatCompletionResponse) {
	t.Helper()
	var v ChatCompletionResponse
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatalf("unmarshal: %v\nsrc=%s", err, src)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out, v
}

// stableJSON re-marshals from the second unmarshal and asserts it equals the
// first re-marshal (i.e. marshal->unmarshal->marshal is a fixed point).
func assertStable(t *testing.T, first []byte, parse func([]byte) (any, error)) {
	t.Helper()
	v, err := parse(first)
	if err != nil {
		t.Fatalf("re-unmarshal: %v\nfirst=%s", err, first)
	}
	second, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("not stable:\n first=%s\nsecond=%s", first, second)
	}
}

func TestChatCompletionRequestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// check inspects the parsed request for semantic survival.
		check func(t *testing.T, r ChatCompletionRequest)
	}{
		{
			name: "string content",
			in:   `{"model":"m","messages":[{"role":"user","content":"hello"}]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				if !r.Messages[0].Content.IsString {
					t.Fatal("IsString lost")
				}
				if r.Messages[0].Content.Text != "hello" {
					t.Fatalf("text=%q", r.Messages[0].Content.Text)
				}
			},
		},
		{
			name: "array-of-parts content",
			in:   `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				if r.Messages[0].Content.IsString {
					t.Fatal("IsString should be false for parts")
				}
				if len(r.Messages[0].Content.Parts) != 1 {
					t.Fatalf("parts=%+v", r.Messages[0].Content.Parts)
				}
			},
		},
		{
			name: "null content",
			// Documents CURRENT behavior: null content round-trips to null.
			in: `{"model":"m","messages":[{"role":"assistant","content":null}]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				if r.Messages[0].Content.IsString {
					t.Fatal("null content must not be a string")
				}
				if len(r.Messages[0].Content.Parts) != 0 {
					t.Fatal("null content must have no parts")
				}
			},
		},
		{
			name: "tool_calls with arguments",
			in:   `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				tc := r.Messages[0].ToolCalls
				if len(tc) != 1 {
					t.Fatalf("tool_calls=%+v", tc)
				}
				if tc[0].Function.Name != "get_weather" {
					t.Fatalf("fn name=%q", tc[0].Function.Name)
				}
				if tc[0].Function.Arguments != `{"city":"SF"}` {
					t.Fatalf("args=%q", tc[0].Function.Arguments)
				}
			},
		},
		{
			name: "assistant with both content and tool_calls",
			in:   `{"model":"m","messages":[{"role":"assistant","content":"thinking","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				m := r.Messages[0]
				if !m.Content.IsString || m.Content.Text != "thinking" {
					t.Fatalf("content=%+v", m.Content)
				}
				if len(m.ToolCalls) != 1 {
					t.Fatalf("tool_calls=%+v", m.ToolCalls)
				}
			},
		},
		{
			name: "multi-part with image_url",
			in:   `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://x/y.png","detail":"high"}}]}]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				parts := r.Messages[0].Content.Parts
				if len(parts) != 2 {
					t.Fatalf("parts=%+v", parts)
				}
				if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://x/y.png" {
					t.Fatalf("image=%+v", parts[1].ImageURL)
				}
				if parts[1].ImageURL.Detail != "high" {
					t.Fatalf("detail=%q", parts[1].ImageURL.Detail)
				}
			},
		},
		{
			name: "stop as string",
			in:   `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":"\n\n"}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				if r.Stop == nil || len(r.Stop.Values) != 1 || r.Stop.Values[0] != "\n\n" {
					t.Fatalf("stop=%+v", r.Stop)
				}
			},
		},
		{
			name: "stop as array",
			in:   `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":["a","b"]}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				if r.Stop == nil || len(r.Stop.Values) != 2 {
					t.Fatalf("stop=%+v", r.Stop)
				}
			},
		},
		{
			name: "full request with scalars",
			in:   `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0.7,"top_p":0.9,"max_tokens":256,"stream":true,"stream_options":{"include_usage":true},"seed":42}`,
			check: func(t *testing.T, r ChatCompletionRequest) {
				if r.Temperature == nil || *r.Temperature != 0.7 {
					t.Fatal("temperature lost")
				}
				if !r.Stream {
					t.Fatal("stream lost")
				}
				if r.StreamOptions == nil || !r.StreamOptions.IncludeUsage {
					t.Fatal("stream_options lost")
				}
				if r.Seed == nil || *r.Seed != 42 {
					t.Fatal("seed lost")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, parsed := remarshalReq(t, tc.in)
			tc.check(t, parsed)
			assertStable(t, first, func(b []byte) (any, error) {
				var v ChatCompletionRequest
				err := json.Unmarshal(b, &v)
				return v, err
			})
		})
	}
}

func TestChatCompletionResponseRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, r ChatCompletionResponse)
	}{
		{
			name: "simple text choice",
			in:   `{"id":"id1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			check: func(t *testing.T, r ChatCompletionResponse) {
				if len(r.Choices) != 1 {
					t.Fatalf("choices=%+v", r.Choices)
				}
				if !r.Choices[0].Message.Content.IsString {
					t.Fatal("message IsString lost")
				}
				if r.Choices[0].FinishReason != "stop" {
					t.Fatalf("finish_reason=%q", r.Choices[0].FinishReason)
				}
			},
		},
		{
			name: "choice with tool_calls",
			in:   `{"id":"id2","object":"chat.completion","created":2,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`,
			check: func(t *testing.T, r ChatCompletionResponse) {
				m := r.Choices[0].Message
				if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Arguments != `{"a":1}` {
					t.Fatalf("tool_calls=%+v", m.ToolCalls)
				}
			},
		},
		{
			name: "with usage",
			in:   `{"id":"id3","object":"chat.completion","created":3,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			check: func(t *testing.T, r ChatCompletionResponse) {
				if r.Usage == nil || r.Usage.TotalTokens != 15 {
					t.Fatalf("usage=%+v", r.Usage)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, parsed := remarshalResp(t, tc.in)
			tc.check(t, parsed)
			assertStable(t, first, func(b []byte) (any, error) {
				var v ChatCompletionResponse
				err := json.Unmarshal(b, &v)
				return v, err
			})
		})
	}
}

func TestChatCompletionChunkRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, c ChatCompletionChunk)
	}{
		{
			name: "delta with finish_reason",
			in:   `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
			check: func(t *testing.T, c ChatCompletionChunk) {
				if len(c.Choices) != 1 {
					t.Fatalf("choices=%+v", c.Choices)
				}
				fr := c.Choices[0].FinishReason
				if fr == nil || *fr != "stop" {
					t.Fatalf("finish_reason=%v", fr)
				}
				if c.Choices[0].Delta.Content != "hi" {
					t.Fatalf("delta content=%q", c.Choices[0].Delta.Content)
				}
			},
		},
		{
			name: "delta with null finish_reason",
			in:   `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`,
			check: func(t *testing.T, c ChatCompletionChunk) {
				if c.Choices[0].FinishReason != nil {
					t.Fatalf("finish_reason should be nil, got %v", *c.Choices[0].FinishReason)
				}
			},
		},
		{
			name: "final chunk with usage",
			in:   `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			check: func(t *testing.T, c ChatCompletionChunk) {
				if c.Usage == nil || c.Usage.TotalTokens != 3 {
					t.Fatalf("usage=%+v", c.Usage)
				}
			},
		},
		{
			name: "delta with tool_calls and index",
			in:   `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":null}]}`,
			check: func(t *testing.T, c ChatCompletionChunk) {
				tc := c.Choices[0].Delta.ToolCalls
				if len(tc) != 1 || tc[0].Index == nil || *tc[0].Index != 0 {
					t.Fatalf("tool_calls=%+v", tc)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v ChatCompletionChunk
			if err := json.Unmarshal([]byte(tc.in), &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			first, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			tc.check(t, v)
			assertStable(t, first, func(b []byte) (any, error) {
				var v2 ChatCompletionChunk
				err := json.Unmarshal(b, &v2)
				return v2, err
			})
		})
	}
}

// TestMessageContentIsStringPreserved is an explicit guard that the IsString
// discriminator survives a marshal/unmarshal cycle for both forms.
func TestMessageContentIsStringPreserved(t *testing.T) {
	for _, in := range []string{
		`{"role":"user","content":"plain"}`,
		`{"role":"user","content":[{"type":"text","text":"part"}]}`,
	} {
		var m1 Message
		if err := json.Unmarshal([]byte(in), &m1); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(m1)
		var m2 Message
		if err := json.Unmarshal(b, &m2); err != nil {
			t.Fatal(err)
		}
		if m1.Content.IsString != m2.Content.IsString {
			t.Fatalf("IsString flipped for %s: %v -> %v", in, m1.Content.IsString, m2.Content.IsString)
		}
	}
}

// FuzzMessageContentUnmarshal feeds arbitrary bytes into a Message and ensures
// unmarshal followed by re-marshal never panics. Invalid JSON is skipped.
func FuzzMessageContentUnmarshal(f *testing.F) {
	seeds := []string{
		`{"role":"user","content":"hello"}`,
		`{"role":"user","content":[{"type":"text","text":"hi"}]}`,
		`{"role":"assistant","content":null}`,
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if !json.Valid(data) {
			t.Skip()
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			return // not a Message shape; acceptable
		}
		// Re-marshal must not panic and must produce valid JSON.
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("re-marshal failed: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("re-marshal produced invalid JSON: %s", out)
		}
		// Second cycle must succeed too.
		var m2 Message
		if err := json.Unmarshal(out, &m2); err != nil {
			t.Fatalf("re-unmarshal failed: %v\nout=%s", err, out)
		}
	})
}

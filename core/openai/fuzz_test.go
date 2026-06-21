package openai

import (
	"encoding/json"
	"testing"
)

// FuzzChatRequestDecode fuzzes the primary ingress: decoding an arbitrary
// request body into ChatCompletionRequest must never panic, and any value that
// decodes must re-marshal to valid JSON (round-trip stability of the envelope:
// Stop StringOrArray, ToolChoice, ResponseFormat, tools[].function.parameters).
func FuzzChatRequestDecode(f *testing.F) {
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"stop":["a","b"],"temperature":0.5}`))
	f.Add([]byte(`{"model":"m","stop":"END","tool_choice":"auto","response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"t","type":"function","function":{"name":"g","arguments":"{}"}}]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var req ChatCompletionRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return // invalid input is fine; must not panic
		}
		out, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("re-marshal failed for decoded request: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("re-marshaled request is not valid JSON: %s", out)
		}
		// Decoding our own output must also succeed.
		var again ChatCompletionRequest
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("round-trip decode failed: %v", err)
		}
	})
}

// FuzzStringOrArray fuzzes the string|[]string union used by `stop`.
func FuzzStringOrArray(f *testing.F) {
	f.Add([]byte(`"x"`))
	f.Add([]byte(`["a","b"]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s StringOrArray
		if err := json.Unmarshal(data, &s); err != nil {
			return
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("marshal StringOrArray: %v", err)
		}
	})
}

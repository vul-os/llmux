package openai

import (
	"encoding/json"
	"testing"
)

func TestMessageContentStringRoundTrip(t *testing.T) {
	in := `{"role":"user","content":"hello"}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Content.IsString || m.Content.Text != "hello" {
		t.Fatalf("got %+v", m.Content)
	}
	out, _ := json.Marshal(m)
	if string(out) != `{"role":"user","content":"hello"}` {
		t.Fatalf("roundtrip=%s", out)
	}
}

func TestMessageContentPartsRoundTrip(t *testing.T) {
	in := `{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content.IsString || len(m.Content.Parts) != 2 {
		t.Fatalf("got %+v", m.Content)
	}
	if m.Content.String() != "hi" {
		t.Fatalf("flattened text=%q", m.Content.String())
	}
	if m.Content.Parts[1].ImageURL.URL != "http://x/y.png" {
		t.Fatalf("image url=%q", m.Content.Parts[1].ImageURL.URL)
	}
}

func TestStringOrArray(t *testing.T) {
	var s StringOrArray
	if err := json.Unmarshal([]byte(`"stop"`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Values) != 1 || s.Values[0] != "stop" {
		t.Fatalf("got %+v", s)
	}
	out, _ := json.Marshal(s)
	if string(out) != `"stop"` {
		t.Fatalf("single roundtrip=%s", out)
	}

	var a StringOrArray
	json.Unmarshal([]byte(`["a","b"]`), &a)
	out, _ = json.Marshal(a)
	if string(out) != `["a","b"]` {
		t.Fatalf("array roundtrip=%s", out)
	}
}

func TestRequestPreservesUnknownViaRawNotNeeded(t *testing.T) {
	// Known fields parse; this guards against accidental schema regressions.
	in := `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0.5,"max_tokens":100,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatal(err)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Fatal("temperature not parsed")
	}
	if req.MaxTokens == nil || *req.MaxTokens != 100 {
		t.Fatal("max_tokens not parsed")
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "f" {
		t.Fatal("tools not parsed")
	}
}

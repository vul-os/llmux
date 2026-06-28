package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// ---------------------------------------------------------------------------
// Malformed-request fuzzing on the gateway
// ---------------------------------------------------------------------------

// fuzzServer builds a minimal gateway backed by an always-200 upstream so that
// malformed requests that reach the router/upstream fail cleanly. The focus is
// on whether the gateway stays up and returns a non-500 for every input.
func fuzzServer(t *testing.T) *Server {
	t.Helper()
	up := hardeningOKUpstream(t, nil)
	t.Cleanup(up.Close)
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestMalformedJSONBodiesNeverCrash verifies that a variety of malformed JSON
// request bodies all receive a clean error response (never a panic/500). The
// exact status is not asserted; only absence of a 500 is required.
func TestMalformedJSONBodiesNeverCrash(t *testing.T) {
	s := fuzzServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"null literal", "null"},
		{"json array", "[]"},
		{"bare string", `"hello"`},
		{"unclosed brace", `{"model":"m"`},
		{"truncated", `{"model":"m","messages":[`},
		{"extra comma", `{"model":"m","messages":[],}`},
		{"wrong type messages", `{"model":"m","messages":"not-array"}`},
		{"number model", `{"model":42,"messages":[]}`},
		{"nested no messages", `{"model":"m"}`},
		{"HTML not JSON", "<html><body>not json</body></html>"},
		{"very long model", `{"model":"` + strings.Repeat("a", 100_000) + `","messages":[]}`},
		{"BOM prefix", "\xef\xbb\xbf" + `{"model":"m","messages":[]}`},
		{"invalid utf-8 in content", `{"model":"m","messages":[{"role":"user","content":"` + "\x80\x81\x82" + `"}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on case %q: %v", tc.name, r)
				}
			}()
			rec := post(s, tc.body)
			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("case %q returned 500: %s", tc.name, rec.Body.String())
			}
		})
	}
}

// TestNullByteInModelFieldNeverCrash verifies that a model field containing
// embedded NUL bytes (passed as Go escape sequence) does not crash the server.
func TestNullByteInModelFieldNeverCrash(t *testing.T) {
	s := fuzzServer(t)
	// Use fmt.Sprintf + %q so the NUL is properly encoded in JSON.
	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`,
		"model"+string([]byte{0x00})+"with"+string([]byte{0x00})+"nulls")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on null byte in model: %v", r)
		}
	}()
	rec := post(s, payload)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("null byte in model returned 500: %s", rec.Body.String())
	}
}

// TestEmptyMessagesArrayIsAccepted verifies that an empty messages array does
// not crash the server (the upstream decides validity, not the gateway).
func TestEmptyMessagesArrayIsAccepted(t *testing.T) {
	s := fuzzServer(t)
	rec := post(s, `{"model":"m","messages":[]}`)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("empty messages returned 500: %s", rec.Body.String())
	}
}

// TestDeeplyNestedJSONNeverCrash verifies that a deeply nested (but
// structurally valid) JSON body does not cause a stack overflow or panic.
func TestDeeplyNestedJSONNeverCrash(t *testing.T) {
	s := fuzzServer(t)

	// Build {"model":"m","messages":[{"role":"user","content":{500 levels deep}}]}
	const depth = 500
	var nested strings.Builder
	for i := 0; i < depth; i++ {
		nested.WriteString(`{"k":`)
	}
	nested.WriteString(`"leaf"`)
	for i := 0; i < depth; i++ {
		nested.WriteString(`}`)
	}
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s}]}`, nested.String())

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on deeply nested JSON: %v", r)
		}
	}()
	rec := post(s, body)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("deeply nested JSON returned 500: %s", rec.Body.String())
	}
}

// TestGiantModelNameNeverCrash verifies that an extremely long model name does
// not crash or OOM the server. The router will fail to resolve it and return a
// clean error.
func TestGiantModelNameNeverCrash(t *testing.T) {
	s := fuzzServer(t)

	giant := strings.Repeat("x", 1<<20) // 1 MiB model name
	body := fmt.Sprintf(`{"model":%q,"messages":[]}`, giant)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on giant model name: %v", r)
		}
	}()
	rec := post(s, body)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("giant model name returned 500: %s", rec.Body.String())
	}
}

// TestStreamFlagNonBooleanNeverCrash verifies that non-boolean values for the
// "stream" field do not crash the gateway.
func TestStreamFlagNonBooleanNeverCrash(t *testing.T) {
	s := fuzzServer(t)

	cases := []string{
		`{"model":"m","messages":[],"stream":"true"}`,
		`{"model":"m","messages":[],"stream":1}`,
		`{"model":"m","messages":[],"stream":null}`,
		`{"model":"m","messages":[],"stream":{}}`,
	}
	for _, body := range cases {
		body := body
		t.Run(body, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			rec := post(s, body)
			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("non-boolean stream returned 500: %s", rec.Body.String())
			}
		})
	}
}

// TestContentTypeVariantsNeverCrash verifies that unexpected Content-Type
// headers don't crash the gateway. The server always parses as JSON.
func TestContentTypeVariantsNeverCrash(t *testing.T) {
	s := fuzzServer(t)

	contentTypes := []string{
		"",
		"text/plain",
		"application/x-www-form-urlencoded",
		"text/html; charset=utf-8",
		"application/json; charset=utf-8; boundary=xxx",
	}
	body := `{"model":"m","messages":[]}`
	for _, ct := range contentTypes {
		ct := ct
		t.Run(ct, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic with content-type %q: %v", ct, r)
				}
			}()
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("content-type %q returned 500: %s", ct, rec.Body.String())
			}
		})
	}
}

// TestHTTPMethodsOnChatEndpoint verifies that non-POST methods on the chat
// endpoint are handled gracefully (no crash, no 500).
func TestHTTPMethodsOnChatEndpoint(t *testing.T) {
	s := fuzzServer(t)

	methods := []string{"GET", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}
	for _, m := range methods {
		m := m
		t.Run(m, func(t *testing.T) {
			req := httptest.NewRequest(m, "/v1/chat/completions", nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("method %s returned 500", m)
			}
		})
	}
}

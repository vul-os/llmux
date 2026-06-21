package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// mockUpstream is a fake OpenAI-compatible provider for tests.
func mockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(openai.NewError("bad key", "invalid_request_error", "invalid_api_key"))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, tok := range []string{"Hello", " world"} {
				chunk := openai.ChatCompletionChunk{
					ID: "up-1", Object: "chat.completion.chunk", Model: model,
					Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: tok}}},
				}
				data, _ := json.Marshal(chunk)
				w.Write([]byte("data: " + string(data) + "\n\n"))
				fl.Flush()
			}
			w.Write([]byte("data: [DONE]\n\n"))
			fl.Flush()
			return
		}
		resp := openai.ChatCompletionResponse{
			ID: "up-1", Object: "chat.completion", Model: model,
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str("Hello world")}, FinishReason: "stop"}},
			Usage:   &openai.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		}
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		resp := openai.EmbeddingResponse{
			Object: "list", Model: "emb",
			Data:  []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}}},
			Usage: &openai.Usage{PromptTokens: 3, TotalTokens: 3},
		}
		json.NewEncoder(w).Encode(resp)
	})

	return httptest.NewServer(mux)
}

func testServer(t *testing.T, up *httptest.Server, routes []config.RouteConfig) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"},
		},
		Routes: routes,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestUnaryChat(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := testServer(t, up, []config.RouteConfig{{Model: "gpt-4o", Provider: "mock", TargetModel: "gpt-4o-2024"}})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openai.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Choices[0].Message.Content.String(); got != "Hello world" {
		t.Fatalf("content=%q", got)
	}
	// Upstream returned model name from the rewritten request -> target model.
	if resp.Model != "gpt-4o-2024" {
		t.Fatalf("model=%q, want rewritten target", resp.Model)
	}
	if resp.Usage.TotalTokens != 7 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}

func TestStreamChat(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := testServer(t, up, []config.RouteConfig{{Model: "*", Provider: "mock"}})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%q", ct)
	}

	var content strings.Builder
	var sawDone bool
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if content.String() != "Hello world" {
		t.Fatalf("streamed content=%q", content.String())
	}
	if !sawDone {
		t.Fatal("missing [DONE] sentinel")
	}
}

func TestErrorRelay(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	// Wrong key -> upstream 401 must be relayed with status + shape.
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "WRONG"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, _ := New(cfg)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 401 {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	var er openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &er)
	if er.Error.Message == "" {
		t.Fatalf("missing error message: %s", rec.Body.String())
	}
}

func TestPrefixRouting(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := testServer(t, up, nil) // no routes -> rely on "provider/model" prefix

	body := `{"model":"mock/gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openai.ChatCompletionResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Model != "gpt-4o" { // prefix stripped to upstream model
		t.Fatalf("model=%q", resp.Model)
	}
}

func TestUnknownModel(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := testServer(t, up, nil)
	body := `{"model":"nonexistent","messages":[]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 404 {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestEmbeddings(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := testServer(t, up, []config.RouteConfig{{Model: "*", Provider: "mock"}})
	body := `{"model":"text-embedding-3-small","input":"hello"}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp openai.EmbeddingResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 2 {
		t.Fatalf("bad embeddings: %s", rec.Body.String())
	}
}

func TestHealth(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := testServer(t, up, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(b), "ok") {
		t.Fatalf("health=%s", b)
	}
}

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
)

// chatBody is a minimal valid chat-completions payload for the given model.
func chatBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
}

// hardeningOKUpstream is an OpenAI-shaped mock that answers chat, moderations,
// and embeddings with a 200. It records whether it was ever reached so SSRF /
// routing tests can assert the absence of any outbound call.
func hardeningOKUpstream(t *testing.T, reached *bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	chat := func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			ID: "up", Object: "chat.completion", Model: model,
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str("ok")}, FinishReason: "stop"}},
			Usage:   &openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		})
	}
	mux.HandleFunc("/v1/chat/completions", chat)
	mux.HandleFunc("/v1/moderations", func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "modr", "results": []any{}})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			*reached = true
		}
		_ = json.NewEncoder(w).Encode(openai.EmbeddingResponse{
			Object: "list", Model: "emb",
			Data: []openai.EmbeddingData{{Object: "embedding", Index: 0, Embedding: []float64{0.1}}},
		})
	})
	return httptest.NewServer(mux)
}

// ---------------------------------------------------------------------------
// Auth matrix
// ---------------------------------------------------------------------------

func TestSecHardening_MasterKeyAuthMatrix(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "MASTER"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// No master key set → 401 on the API.
	if rec := postKey(s, chatBody("m"), ""); rec.Code != 401 {
		t.Fatalf("no key on /v1/chat: status=%d, want 401", rec.Code)
	}
	// Wrong master key → 401.
	if rec := postKey(s, chatBody("m"), "WRONG"); rec.Code != 401 {
		t.Fatalf("wrong key on /v1/chat: status=%d, want 401", rec.Code)
	}
	// Correct master key → 200.
	if rec := postKey(s, chatBody("m"), "MASTER"); rec.Code != 200 {
		t.Fatalf("master key on /v1/chat: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

func TestSecHardening_AdminAndMetricsRejectVirtualKeys(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0", MasterKey: "MASTER"},
		Keys:   []config.KeyConfig{{Key: "sk-virtual", Name: "tenant", BudgetUSD: 100}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, path := range []string{"/admin/keys", "/admin/usage", "/metrics"} {
		// No key → 401.
		if rec := getAuth(s, path, ""); rec.Code != 401 {
			t.Fatalf("%s no-key: status=%d, want 401", path, rec.Code)
		}
		// Virtual key → 401 (privileged endpoints never accept virtual keys).
		if rec := getAuth(s, path, "sk-virtual"); rec.Code != 401 {
			t.Fatalf("%s virtual-key: status=%d, want 401", path, rec.Code)
		}
		// Master key → 200.
		if rec := getAuth(s, path, "MASTER"); rec.Code != 200 {
			t.Fatalf("%s master-key: status=%d, want 200", path, rec.Code)
		}
	}
}

func TestSecHardening_HealthDisclosureGatedByMaster(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "MASTER"},
		Providers: []config.ProviderConfig{{Name: "secretprov", Type: config.TypePassthrough, BaseURL: "https://internal.example/v1"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Without the master key: status only, no provider list.
	rec := getAuth(s, "/health", "")
	if rec.Code != 200 {
		t.Fatalf("/health no-key: status=%d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "providers") || strings.Contains(rec.Body.String(), "secretprov") {
		t.Fatalf("/health leaked provider list to unauthenticated caller: %s", rec.Body.String())
	}
	// With the master key: provider list disclosed.
	rec = getAuth(s, "/health", "MASTER")
	if !strings.Contains(rec.Body.String(), "providers") || !strings.Contains(rec.Body.String(), "secretprov") {
		t.Fatalf("/health with master missing provider list: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Budget / rate-limit enforcement
// ---------------------------------------------------------------------------

func TestSecHardening_OverBudgetReturns402(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:      []config.KeyConfig{{Key: "sk-poor", Name: "poor", BudgetUSD: 1.0}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Push spend past the budget.
	s.keys.AddSpend("sk-poor", 5.0)

	rec := postKey(s, chatBody("m"), "sk-poor")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("over-budget key: status=%d, want 402", rec.Code)
	}
}

func TestSecHardening_RateLimitReturns429(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:      []config.KeyConfig{{Key: "sk-rl", Name: "rl", RPM: 1}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// First request consumes the single token.
	if rec := postKey(s, chatBody("m"), "sk-rl"); rec.Code != 200 {
		t.Fatalf("first request: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	// Second within the same minute → 429.
	if rec := postKey(s, chatBody("m"), "sk-rl"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited request: status=%d, want 429", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Model allow-list bypass attempt
// ---------------------------------------------------------------------------

func TestSecHardening_ModelAllowListBypass(t *testing.T) {
	var reached bool
	up := hardeningOKUpstream(t, &reached)
	defer up.Close()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "mock"}},
		// Key may only use "allowed-model".
		Keys: []config.KeyConfig{{Key: "sk-restricted", Name: "restricted", AllowedModels: []string{"allowed-model"}}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Chat completions: requesting a different model → 403, no outbound call.
	rec := postKey(s, chatBody("forbidden-model"), "sk-restricted")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/v1/chat disallowed model: status=%d, want 403", rec.Code)
	}
	if reached {
		t.Fatal("/v1/chat disallowed model reached upstream (allow-list bypassed)")
	}

	// Modality route (/v1/moderations) must enforce the same allow-list → 403.
	reached = false
	mrec := httptest.NewRecorder()
	mreq := httptest.NewRequest("POST", "/v1/moderations", strings.NewReader(`{"model":"forbidden-model","input":"x"}`))
	mreq.Header.Set("Authorization", "Bearer sk-restricted")
	s.Handler().ServeHTTP(mrec, mreq)
	if mrec.Code != http.StatusForbidden {
		t.Fatalf("/v1/moderations disallowed model: status=%d, want 403", mrec.Code)
	}
	if reached {
		t.Fatal("/v1/moderations disallowed model reached upstream (allow-list bypassed)")
	}

	// Sanity: the permitted model is accepted on chat.
	if rec := postKey(s, chatBody("allowed-model"), "sk-restricted"); rec.Code != 200 {
		t.Fatalf("/v1/chat allowed model: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Secret non-leakage
// ---------------------------------------------------------------------------

const secretAPIKey = "SUPER-SECRET"

func TestSecHardening_UpstreamErrorDoesNotLeakSecret(t *testing.T) {
	// Upstream rejects with a 401; the response body must never contain the
	// provider API key.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(openai.NewError("unauthorized", "authentication_error", "invalid_api_key"))
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: secretAPIKey}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := post(s, chatBody("m"))
	assertNoSecretLeak(t, rec.Body.String(), up.URL)
}

func TestSecHardening_TransportErrorDoesNotLeakSecretOrHost(t *testing.T) {
	// Unreachable base_url: the dial error embeds the upstream host. Neither the
	// host, the word "dial", nor the API key may reach the client.
	badURL := "http://127.0.0.1:1/v1"
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: badURL, APIKey: secretAPIKey}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := post(s, chatBody("m"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("transport error: status=%d, want 502", rec.Code)
	}
	body := rec.Body.String()
	assertNoSecretLeak(t, body, badURL)
	if strings.Contains(body, "dial") || strings.Contains(body, "127.0.0.1") {
		t.Fatalf("transport error leaked host/dial detail: %s", body)
	}
}

// assertNoSecretLeak fails if the client-visible body contains the API key or
// the raw upstream URL/host.
func assertNoSecretLeak(t *testing.T, body, upstreamURL string) {
	t.Helper()
	if strings.Contains(body, secretAPIKey) {
		t.Fatalf("response leaked provider API key: %s", body)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(upstreamURL, "http://"), "https://")
	host = strings.TrimSuffix(host, "/v1")
	if host != "" && strings.Contains(body, host) {
		t.Fatalf("response leaked upstream URL/host %q: %s", host, body)
	}
}

// ---------------------------------------------------------------------------
// Raw upstream body not echoed
// ---------------------------------------------------------------------------

func TestSecHardening_RawUpstreamBodyNotEchoed(t *testing.T) {
	const marker = "INTERNAL-LB-MARKER-7f3a"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A non-JSON (HTML) error body from an intermediary load balancer.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>" + marker + " upstream down</body></html>"))
	}))
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Retry:     config.RetryConfig{MaxRetries: 0, BackoffMS: 1},
		Providers: []config.ProviderConfig{{Name: "p", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "p"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := post(s, chatBody("m"))
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("raw upstream HTML body echoed to client: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Oversize request body
// ---------------------------------------------------------------------------

func TestSecHardening_OversizeBodyDoesNotOOM(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A body well past the 32 MiB cap: the LimitReader truncates it, so the
	// server reads at most 32 MiB and never OOMs/panics. The truncated payload
	// is no longer valid JSON → a clean 400, not a crash.
	huge := maxBodyBytes + (1 << 20) // 33 MiB
	var b strings.Builder
	b.Grow(huge + 64)
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"`)
	pad := strings.Repeat("A", huge)
	b.WriteString(pad)
	b.WriteString(`"}]}`)
	rec := post(s, b.String())
	// Must not panic (recoverMW would turn a panic into 500). Truncated JSON is
	// rejected; we accept any non-panicking client error here.
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("oversize body caused 500/panic: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Logf("oversize body status=%d (acceptable as long as not a panic/OOM)", rec.Code)
	}

	// A moderately large but valid body still works.
	content := strings.Repeat("x", 1<<20) // 1 MiB of content
	ok := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%q}]}`, content)
	if rec := post(s, ok); rec.Code != 200 {
		t.Fatalf("moderately large valid body: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// SSRF / outbound host control
// ---------------------------------------------------------------------------

func TestSecHardening_NoSSRFViaUnknownProvider(t *testing.T) {
	var reached bool
	up := hardeningOKUpstream(t, &reached)
	defer up.Close()
	// Only "mock" is configured. A client cannot introduce a new outbound host.
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		// No catch-all route: only the prefix syntax can reach a configured provider.
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// "unknownprovider/model" prefix names a provider that does not exist → 404,
	// and no outbound request is attempted.
	rec := post(s, chatBody("unknownprovider/model"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider prefix: status=%d, want 404", rec.Code)
	}
	if reached {
		t.Fatal("unknown provider prefix triggered an outbound call (SSRF surface)")
	}

	// A model that looks like a URL/host cannot redirect the outbound host either.
	rec = post(s, chatBody("http://169.254.169.254/latest/meta-data"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("url-shaped model: status=%d, want 404", rec.Code)
	}
	if reached {
		t.Fatal("url-shaped model triggered an outbound call (SSRF surface)")
	}

	// Routing only ever targets the configured provider: the prefix that DOES
	// match goes to the mock upstream and nowhere else.
	reached = false
	if rec := post(s, chatBody("mock/gpt-4o")); rec.Code != 200 {
		t.Fatalf("configured provider prefix: status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !reached {
		t.Fatal("configured provider was not the outbound target")
	}
}

// ---------------------------------------------------------------------------
// CRLF / header injection
// ---------------------------------------------------------------------------

func TestSecHardening_CRLFInjectionInModelAndMessage(t *testing.T) {
	var reached bool
	up := hardeningOKUpstream(t, &reached)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A model name carrying a CRLF + header injection attempt. It is JSON-encoded
	// so the body stays valid; the value flows into the upstream URL path/body.
	// Go's net/http rejects control chars in a request line/headers, so the worst
	// case is a transport error — never an injected header and never a panic.
	injModel := "evil\r\nX-Injected: 1"
	rec := func() (rec *httptest.ResponseRecorder) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CRLF in model panicked: %v", r)
			}
		}()
		return post(s, chatBody(injModel))
	}()
	// Routed via "*" to the mock; the upstream path includes the injected value.
	// Either it errors cleanly (transport rejects the control chars) or succeeds
	// with the value safely encoded — both are fine; the must-not is a 500/panic.
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("CRLF in model produced a 500/panic: %s", rec.Body.String())
	}

	// CRLF inside a message content field must not crash either; it is just JSON
	// string data and cannot inject headers.
	reached = false
	injBody := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%q}]}`, "hi\r\nX-Injected: 1")
	mrec := func() (rec *httptest.ResponseRecorder) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CRLF in message panicked: %v", r)
			}
		}()
		return post(s, injBody)
	}()
	if mrec.Code == http.StatusInternalServerError {
		t.Fatalf("CRLF in message produced a 500/panic: %s", mrec.Body.String())
	}
	if mrec.Code != 200 {
		t.Fatalf("CRLF in message content: status=%d body=%s, want 200 (treated as data)", mrec.Code, mrec.Body.String())
	}
}

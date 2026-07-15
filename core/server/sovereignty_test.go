package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/sovereign"
)

// countingUpstream is an OpenAI-compatible upstream that records how many times
// it was actually dialed — the test uses this to prove a blocked request never
// reaches the network.
func countingUpstream(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		resp := openai.ChatCompletionResponse{
			ID: "up-1", Object: "chat.completion", Model: model,
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: openai.Str("hi")}, FinishReason: "stop"}},
			Usage:   &openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		}
		json.NewEncoder(w).Encode(resp)
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	return httptest.NewServer(mux)
}

func chatReq(model string) []byte {
	b, _ := json.Marshal(openai.ChatCompletionRequest{
		Model: model, Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	})
	return b
}

// buildServer constructs a Server routed to a single provider, optionally
// overriding the sovereignty policy so the (loopback) upstream is classified as
// a remote egress target for the purpose of the test.
func buildServer(t *testing.T, upURL string, egressPolicy *sovereign.Policy) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "mock", Type: config.TypePassthrough, BaseURL: upURL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if egressPolicy != nil {
		s.sovereign = egressPolicy
	}
	return s
}

func doChat(t *testing.T, s *Server, model string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(chatReq(model))))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Result()
}

// TestSovereignLocalDefaultServes proves the sovereign default path works: a
// loopback (on-box) provider is allowed with no opt-in and serves the request.
func TestSovereignLocalDefaultServes(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	// Real policy from config: the loopback upstream classifies as local.
	s := buildServer(t, up.URL, nil)
	if d := s.sovereign.Check("mock"); d.Locality != sovereign.Local || !d.Allowed {
		t.Fatalf("loopback provider should be local+allowed; got %+v", d)
	}
	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local request should succeed; got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("local upstream should have been called once; hits=%d", hits)
	}
}

// TestSovereignBlocksEgressByDefault is the core guarantee: a non-local provider
// that the operator has NOT opted in is denied BEFORE any network call. Nothing
// leaves the box.
func TestSovereignBlocksEgressByDefault(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	// Classify "mock" as a remote egress target WITHOUT opt-in.
	blocked := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: false},
	})
	s := buildServer(t, up.URL, blocked)

	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("egress must be blocked; got status %d", resp.StatusCode)
	}
	var e openai.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "egress_not_allowed" {
		t.Fatalf("expected egress_not_allowed; got code=%q msg=%q", e.Error.Code, e.Error.Message)
	}
	// The whole point: the upstream was never dialed.
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("blocked request must NOT reach the network; upstream hits=%d", n)
	}
	if atomic.LoadInt64(&s.metrics.egressBlocked) != 1 {
		t.Fatalf("blocked egress should increment the metric")
	}
}

// TestSovereignEgressOptInReaches proves the escape hatch is real and explicit:
// the SAME remote provider, once allow_egress is set, is permitted and reaches
// the upstream. Only the opt-in flag differs from the blocked case above.
func TestSovereignEgressOptInReaches(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	allowed := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: true},
	})
	s := buildServer(t, up.URL, allowed)

	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("opted-in egress should succeed; got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("opted-in egress should reach the upstream once; hits=%d", n)
	}
}

// TestSovereignSovereignTierServesWithoutOptIn proves the new tier semantics: a
// provider the operator declared "sovereign" (inside the sovereignty boundary)
// is allowed with NO opt-in flag — same as local — and reaches the upstream.
func TestSovereignSovereignTierServesWithoutOptIn(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	// Off-box URL, marked sovereign, no allow_egress / allow_brokered.
	pol := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://pool.eu.vulos.org/v1", Tier: "sovereign"},
	})
	s := buildServer(t, up.URL, pol)

	if d := s.sovereign.Check("mock"); d.Tier != sovereign.TierSovereign || !d.Allowed {
		t.Fatalf("sovereign provider should be allowed with no opt-in; got %+v", d)
	}
	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sovereign request should succeed; got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("sovereign upstream should have been called once; hits=%d", n)
	}
}

// TestSovereignBrokeredBlockedUntilOptIn proves brokered is default-deny: the
// SAME brokered provider is blocked without opt-in, then reaches the upstream
// once allow_brokered is set. Only the opt-in flag differs.
func TestSovereignBrokeredBlockedUntilOptIn(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	// Blocked: brokered without opt-in.
	blocked := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://broker.example.com/v1", Tier: "brokered"},
	})
	s := buildServer(t, up.URL, blocked)
	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("brokered without opt-in must be blocked; got %d", resp.StatusCode)
	}
	var e openai.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "egress_not_allowed" {
		t.Fatalf("brokered block should use the same 403 shape; got code=%q", e.Error.Code)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("blocked brokered request must NOT reach the network; hits=%d", n)
	}

	// Allowed: same provider, allow_brokered opt-in.
	allowed := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://broker.example.com/v1", Tier: "brokered", AllowBrokered: true},
	})
	s2 := buildServer(t, up.URL, allowed)
	resp2 := doChat(t, s2, "anything")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("opted-in brokered should succeed; got %d", resp2.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("opted-in brokered should reach the upstream once; hits=%d", n)
	}
}

func doForward(t *testing.T, s *Server, path, model string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": "hi"})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Result()
}

// TestSovereignBlocksEgressOnForwardRoutes is a regression for the bypass where
// the modality/forward routes (/v1/completions, /moderations, /rerank, …) did
// NOT run enforceSovereignty and would forward prompts to a blocked, non-opted-in
// remote provider. They must honor the same default-deny egress gate as chat.
func TestSovereignBlocksEgressOnForwardRoutes(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	blocked := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: false},
	})
	s := buildServer(t, up.URL, blocked)

	for _, path := range []string{"/v1/completions", "/v1/moderations", "/v1/rerank"} {
		resp := doForward(t, s, path, "anything")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: forward egress must be blocked; got %d", path, resp.StatusCode)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("blocked forward routes must NOT reach the network; upstream hits=%d", n)
	}
}

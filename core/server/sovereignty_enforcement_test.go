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

// fullUpstream is a counting upstream that answers EVERY modality/embeddings
// suffix (not just chat), so the positive-path tests can prove an opted-in
// forward actually reaches the network. hits counts total dials across paths.
func fullUpstream(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "up-1", "object": "resource", "model": model,
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}
	for _, p := range []string{
		"/v1/chat/completions", "/v1/completions", "/v1/moderations",
		"/v1/images/generations", "/v1/audio/speech", "/v1/rerank",
		"/v1/responses", "/v1/embeddings",
	} {
		mux.HandleFunc(p, ok)
	}
	return httptest.NewServer(mux)
}

// TestSovereignBlocksEveryModalityRoute extends the forward-route regression to
// EVERY model-bearing modality path (the full F4 surface), not just the three
// the original test covered. A blocked, non-opted-in remote provider must be
// denied on images, audio/speech, and responses too — none may open a socket.
func TestSovereignBlocksEveryModalityRoute(t *testing.T) {
	var hits int32
	up := fullUpstream(t, &hits)
	defer up.Close()

	blocked := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: false},
	})
	s := buildServer(t, up.URL, blocked)

	// The complete modality set from forward.go modalityRoutes.
	paths := []string{
		"/v1/completions", "/v1/moderations", "/v1/images/generations",
		"/v1/audio/speech", "/v1/rerank", "/v1/responses",
	}
	for _, path := range paths {
		resp := doForward(t, s, path, "anything")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: modality egress must be blocked; got %d", path, resp.StatusCode)
		}
		var e openai.ErrorResponse
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error.Code != "egress_not_allowed" {
			t.Fatalf("%s: expected egress_not_allowed; got code=%q", path, e.Error.Code)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("blocked modality routes must NOT reach the network; upstream hits=%d", n)
	}
	// One metric increment per blocked route.
	if got := atomic.LoadInt64(&s.metrics.egressBlocked); got != int64(len(paths)) {
		t.Fatalf("egressBlocked = %d, want %d (one per blocked route)", got, len(paths))
	}
}

// TestSovereignModalityOptInReaches is the positive counterpart: with
// allow_egress set, a modality forward is permitted and DOES reach the upstream.
// Proves the gate is a gate, not a wall — opt-in flips it, per route.
func TestSovereignModalityOptInReaches(t *testing.T) {
	var hits int32
	up := fullUpstream(t, &hits)
	defer up.Close()

	allowed := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: true},
	})
	s := buildServer(t, up.URL, allowed)

	for _, path := range []string{"/v1/completions", "/v1/moderations", "/v1/rerank"} {
		resp := doForward(t, s, path, "anything")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: opted-in modality should reach upstream; got %d", path, resp.StatusCode)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Fatalf("opted-in modality routes should reach upstream 3x; hits=%d", n)
	}
}

// doEmbeddings posts an embeddings request through the full handler chain.
func doEmbeddings(t *testing.T, s *Server, model string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "input": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Result()
}

// TestSovereignBlocksEmbeddings proves the embeddings route (handleEmbeddings)
// is gated: a blocked remote provider is denied BEFORE the embeddings call, so
// no vectors and no input text leave the box.
func TestSovereignBlocksEmbeddings(t *testing.T) {
	var hits int32
	up := fullUpstream(t, &hits)
	defer up.Close()

	blocked := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: false},
	})
	s := buildServer(t, up.URL, blocked)

	resp := doEmbeddings(t, s, "anything")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("embeddings egress must be blocked; got %d", resp.StatusCode)
	}
	var e openai.ErrorResponse
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "egress_not_allowed" {
		t.Fatalf("embeddings block should use egress_not_allowed; got code=%q", e.Error.Code)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("blocked embeddings must NOT reach the network; hits=%d", n)
	}
}

// TestSovereignEmbeddingsOptInReaches proves the same embeddings route serves
// once allow_egress is set — only the opt-in differs.
func TestSovereignEmbeddingsOptInReaches(t *testing.T) {
	var hits int32
	up := fullUpstream(t, &hits)
	defer up.Close()

	allowed := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: true},
	})
	s := buildServer(t, up.URL, allowed)

	resp := doEmbeddings(t, s, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("opted-in embeddings should reach upstream; got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("opted-in embeddings should reach upstream once; hits=%d", n)
	}
}

// TestSovereignStreamChatBlocked proves streaming chat (streamChat) is gated
// too: a blocked remote provider is skipped before ChatCompletionStream, and
// with no other target the 403 surfaces without opening a stream.
func TestSovereignStreamChatBlocked(t *testing.T) {
	var hits int32
	up := fullUpstream(t, &hits)
	defer up.Close()

	blocked := sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "mock", BaseURL: "https://api.remote.example/v1", AllowEgress: false},
	})
	s := buildServer(t, up.URL, blocked)

	body, _ := json.Marshal(openai.ChatCompletionRequest{
		Model: "anything", Stream: true,
		Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	resp := w.Result()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked streaming chat must be denied; got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("blocked streaming chat must NOT reach the network; hits=%d", n)
	}
}

// TestSovereignBlocksSemanticCacheEmbedder is the regression for the semantic
// cache egress bypass: the semantic cache embeds EVERY chat prompt (on both
// lookup and store) to compute its similarity key. If the configured embed
// model routes to a non-local provider, the prompt text would silently egress
// on every request — even one served by a purely local chat provider — because
// the embedder called Provider.Embeddings directly, without the dispatch-time
// sovereignty gate. This proves the embedder is now gated: a chat served by a
// LOCAL provider must NOT cause the (blocked, remote) embed model to be dialed,
// and no prompt text leaves the box via the cache. The request still succeeds
// (an embed error is a cache miss, so caching simply no-ops), and the local
// chat upstream serves it.
func TestSovereignBlocksSemanticCacheEmbedder(t *testing.T) {
	var chatHits, embedHits int32
	chatUp := countingUpstream(t, &chatHits) // local chat provider (allowed)
	defer chatUp.Close()
	embedUp := fullUpstream(t, &embedHits) // remote embed provider (blocked)
	defer embedUp.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "localchat", Type: config.TypePassthrough, BaseURL: chatUp.URL + "/v1"},
			{Name: "remoteembed", Type: config.TypePassthrough, BaseURL: embedUp.URL + "/v1"},
		},
		Routes: []config.RouteConfig{
			{Model: "chat-model", Provider: "localchat"},
			{Model: "embed-model", Provider: "remoteembed"},
		},
		Cache: config.CacheConfig{Semantic: true, EmbeddingModel: "embed-model"},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Policy: the local chat provider is allowed; the embed provider is a blocked
	// external egress target (not opted in), even though its URL is loopback.
	s.sovereign = sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "localchat", BaseURL: chatUp.URL + "/v1"},               // loopback → local, allowed
		{Name: "remoteembed", BaseURL: "https://api.embed.example/v1"}, // external, blocked
	})

	// A normal chat request. With semantic caching on, the cache tries to embed
	// the prompt on lookup (and would on store) — that embed must be blocked.
	resp := doChat(t, s, "chat-model")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local chat should still be served; got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&chatHits); n != 1 {
		t.Fatalf("local chat upstream should serve once; hits=%d", n)
	}
	// The core guarantee: the blocked, remote embed model was NEVER dialed, so no
	// prompt text left the box via the semantic cache.
	if n := atomic.LoadInt32(&embedHits); n != 0 {
		t.Fatalf("semantic-cache embedder must NOT reach the blocked remote embed provider; hits=%d", n)
	}
}

// TestSovereignSemanticCacheEmbedderOptInReaches is the positive counterpart:
// once the embed provider is opted in (allow_egress), the semantic cache is
// allowed to embed prompts through it — proving the embedder gate is a gate,
// not a hard wall.
func TestSovereignSemanticCacheEmbedderOptInReaches(t *testing.T) {
	var chatHits, embedHits int32
	chatUp := countingUpstream(t, &chatHits)
	defer chatUp.Close()
	embedUp := fullUpstream(t, &embedHits)
	defer embedUp.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "localchat", Type: config.TypePassthrough, BaseURL: chatUp.URL + "/v1"},
			{Name: "remoteembed", Type: config.TypePassthrough, BaseURL: embedUp.URL + "/v1"},
		},
		Routes: []config.RouteConfig{
			{Model: "chat-model", Provider: "localchat"},
			{Model: "embed-model", Provider: "remoteembed"},
		},
		Cache: config.CacheConfig{Semantic: true, EmbeddingModel: "embed-model"},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.sovereign = sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "localchat", BaseURL: chatUp.URL + "/v1"},
		{Name: "remoteembed", BaseURL: "https://api.embed.example/v1", AllowEgress: true}, // opted in
	})

	resp := doChat(t, s, "chat-model")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local chat should be served; got %d", resp.StatusCode)
	}
	// The opted-in embed provider IS dialed (at least once, for the lookup embed).
	if n := atomic.LoadInt32(&embedHits); n < 1 {
		t.Fatalf("opted-in semantic-cache embedder should reach the embed provider; hits=%d", n)
	}
}

// buildFailoverServer wires a primary provider (blocked/remote by policy) with a
// local fallback, so a test can prove dispatch SKIPS the blocked target and
// still serves from the on-box fallback — the sovereignty gate must not break
// legitimate local failover.
func buildFailoverServer(t *testing.T, localURL string) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "remote", Type: config.TypePassthrough, BaseURL: "https://api.remote.example/v1"},
			{Name: "local", Type: config.TypePassthrough, BaseURL: localURL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "remote", Fallbacks: []string{"local"}}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Policy: primary "remote" is a blocked external egress; "local" is the
	// real loopback upstream and is allowed.
	s.sovereign = sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "remote", BaseURL: "https://api.remote.example/v1"}, // blocked
		{Name: "local", BaseURL: localURL + "/v1"},                 // loopback → local, allowed
	})
	return s
}

// TestSovereignFailoverSkipsBlockedServesLocal is the subtle correctness case:
// when the PRIMARY target is a blocked remote endpoint but a LOCAL fallback
// exists, dispatch must (a) never dial the blocked remote and (b) still serve
// the request from the local fallback. The gate protects sovereignty without
// killing legitimate on-box failover.
func TestSovereignFailoverSkipsBlockedServesLocal(t *testing.T) {
	var hits int32
	local := countingUpstream(t, &hits) // only /v1/chat/completions, which is what we call
	defer local.Close()

	s := buildFailoverServer(t, local.URL)

	// Sanity: the policy says remote is blocked, local is allowed.
	if d := s.sovereign.Check("remote"); d.Allowed {
		t.Fatalf("remote should be blocked; got %+v", d)
	}
	if d := s.sovereign.Check("local"); !d.Allowed || d.Tier != sovereign.TierLocal {
		t.Fatalf("local should be allowed+local; got %+v", d)
	}

	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request should be served by the local fallback; got %d", resp.StatusCode)
	}
	// The local fallback served exactly once; the blocked remote was never dialed
	// (it points at a non-routable example host — if it were dialed the test would
	// hang/error rather than return 200 quickly).
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("local fallback should serve once; hits=%d", n)
	}
}

// TestSovereignAllTargetsBlockedSurfaces403 proves the flip side: when BOTH the
// primary and the fallback are blocked egress, no fallback can serve, so the
// 403 surfaces and nothing is dialed.
func TestSovereignAllTargetsBlockedSurfaces403(t *testing.T) {
	var hits int32
	up := countingUpstream(t, &hits)
	defer up.Close()

	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "remote1", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"},
			{Name: "remote2", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "remote1", Fallbacks: []string{"remote2"}}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Both classified as blocked external egress regardless of the loopback URL.
	s.sovereign = sovereign.NewPolicy([]config.ProviderConfig{
		{Name: "remote1", BaseURL: "https://api.a.example/v1"},
		{Name: "remote2", BaseURL: "https://api.b.example/v1"},
	})

	resp := doChat(t, s, "anything")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("all-blocked dispatch must surface 403; got %d", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("all-blocked dispatch must NOT dial anything; hits=%d", n)
	}
}

// TestHealthDisclosesSovereigntyPosture exercises the /health disclosure surface
// (Decisions/TierSummary/AllowedEgress/Label wired into the admin health JSON):
// the posture must honestly report each provider's tier and whether it may
// egress, and group allowed providers by tier.
func TestHealthDisclosesSovereigntyPosture(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0", MasterKey: "master"},
		Providers: []config.ProviderConfig{
			{Name: "ollama", Type: config.TypePassthrough, BaseURL: "http://localhost:11434/v1"},
			{Name: "openai", Type: config.TypePassthrough, BaseURL: "https://api.openai.com/v1"}, // blocked
			{Name: "pool", Type: config.TypePassthrough, BaseURL: "https://pool.eu.vulos.org/v1", Tier: "sovereign"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "ollama"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", w.Code)
	}

	var out struct {
		Providers []struct {
			Name          string `json:"name"`
			Tier          string `json:"tier"`
			TierLabel     string `json:"tier_label"`
			EgressAllowed bool   `json:"egress_allowed"`
		} `json:"providers"`
		Sovereignty struct {
			Default       string              `json:"default"`
			Tiers         map[string][]string `json:"tiers"`
			EgressAllowed []string            `json:"egress_allowed"`
		} `json:"sovereignty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode health: %v", err)
	}

	byName := map[string]struct {
		tier    string
		label   string
		allowed bool
	}{}
	for _, p := range out.Providers {
		byName[p.Name] = struct {
			tier    string
			label   string
			allowed bool
		}{p.Tier, p.TierLabel, p.EgressAllowed}
	}
	if d := byName["ollama"]; d.tier != "local" || !d.allowed || d.label != "On your device" {
		t.Errorf("ollama posture = %+v, want local/allowed/On your device", d)
	}
	if d := byName["openai"]; d.tier != "external" || d.allowed || d.label != "External · not private" {
		t.Errorf("openai posture = %+v, want external/blocked/External label", d)
	}
	if d := byName["pool"]; d.tier != "sovereign" || !d.allowed {
		t.Errorf("pool posture = %+v, want sovereign/allowed", d)
	}
	if out.Sovereignty.Default != "local" {
		t.Errorf("sovereignty.default = %q, want local", out.Sovereignty.Default)
	}
	// TierSummary groups allowed providers; the blocked openai must be absent.
	if got := out.Sovereignty.Tiers["external"]; len(got) != 0 {
		t.Errorf("external tier should be empty (openai blocked); got %v", got)
	}
	if got := out.Sovereignty.Tiers["local"]; len(got) != 1 || got[0] != "ollama" {
		t.Errorf("local tier = %v, want [ollama]", got)
	}
	if got := out.Sovereignty.Tiers["sovereign"]; len(got) != 1 || got[0] != "pool" {
		t.Errorf("sovereign tier = %v, want [pool]", got)
	}
	// pool is an allowed egress; openai (blocked) must not appear.
	for _, name := range out.Sovereignty.EgressAllowed {
		if name == "openai" {
			t.Errorf("blocked openai leaked into egress_allowed: %v", out.Sovereignty.EgressAllowed)
		}
	}
}

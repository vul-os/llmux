package cp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/server"
)

// ---------------------------------------------------------------------------
// End-to-end billing path: gateway request → cp usage POST
// ---------------------------------------------------------------------------

// newE2EServer builds a gateway server wired with an external stub identity
// and the given usage logger so that authenticated chat requests emit usage.
func newE2EServer(t *testing.T, llmURL string, logger server.UsageLogger) *server.Server {
	t.Helper()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: llmURL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, err := server.New(cfg)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	// Stub external identity so the account id flows into usage records.
	s.SetIdentity(stubIdentity{account: "billing-test-acct"})
	s.SetBudgetGate(stubBudget{})
	s.SetUsageLogger(logger)
	return s
}

// stubIdentity satisfies server.Identity for tests.
type stubIdentity struct{ account string }

func (si stubIdentity) Resolve(_ context.Context, token string) (server.Principal, bool) {
	return server.Principal{Token: token, AccountID: si.account, Tier: "test"}, true
}

// stubBudget satisfies server.BudgetGate for tests.
type stubBudget struct{}

func (stubBudget) Check(_ context.Context, _ server.Principal) server.BudgetDecision {
	return server.BudgetDecision{}
}

// chatPayload is a minimal chat request body.
const chatPayload = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

// mockLLM returns a minimal chat completion response with the given token counts.
func mockLLM(prompt, completion int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "model": "gpt-4o",
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": prompt, "completion_tokens": completion,
				"total_tokens": prompt + completion,
			},
		})
	}))
}

// TestBillingPathGatewayToCP verifies the full billing seam: gateway request
// → metered by UsageLogger → cp usage POST arrives at the mock cp endpoint.
func TestBillingPathGatewayToCP(t *testing.T) {
	llmUp := mockLLM(100, 50)
	defer llmUp.Close()

	var mu sync.Mutex
	var cpPosts []map[string]any
	cpDone := make(chan struct{}, 1)
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage" {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		mu.Lock()
		cpPosts = append(cpPosts, body)
		mu.Unlock()
		select {
		case cpDone <- struct{}{}:
		default:
		}
	}))
	defer cpSrv.Close()

	cpLogger := NewUsageLogger(New(cpSrv.URL, "billing-secret"))
	cl := &countLogger{}
	multi := NewMultiUsageLogger(cl, cpLogger)
	s := newE2EServer(t, llmUp.URL, multi)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatPayload))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Local logger sees the record immediately.
	if cl.n != 1 {
		t.Fatalf("local logger: %d records, want 1", cl.n)
	}

	// cp POST must arrive within 5 s (fire-and-forget worker).
	select {
	case <-cpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cp usage POST never arrived")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cpPosts) == 0 {
		t.Fatal("no cp usage POST received")
	}
	post := cpPosts[0]
	if post["product"] != "llm" {
		t.Fatalf("product=%v, want llm", post["product"])
	}
	if post["account_id"] != "billing-test-acct" {
		t.Fatalf("account_id=%v, want billing-test-acct", post["account_id"])
	}
	if post["kind"] != "llm_tokens" {
		t.Fatalf("kind=%v, want llm_tokens", post["kind"])
	}
	if count, ok := post["count"].(float64); !ok || count == 0 {
		t.Fatalf("count=%v (want non-zero)", post["count"])
	}
}

// TestBillingPathBYOKSkipsCP verifies that a BYOK request (served with the
// account's own provider key) is NOT billed to cp: the cp usage logger must
// receive no usage POST for BYOK-served requests.
func TestBillingPathBYOKSkipsCP(t *testing.T) {
	var seenKey string
	var mu sync.Mutex
	llmUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenKey = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion", "model": "gpt-4o",
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer llmUp.Close()

	var cpHits int32
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage" {
			atomic.AddInt32(&cpHits, 1)
		}
	}))
	defer cpSrv.Close()

	cpLogger := NewUsageLogger(New(cpSrv.URL, ""))
	s := newE2EServer(t, llmUp.URL, cpLogger)

	// Wire a BYOK store with an account key so the request uses BYOK.
	byokStore := &fakeByok{m: map[string]map[string]string{
		"billing-test-acct": {"openai": "byok-account-key"},
	}}
	s.SetBYOKStore(byokStore)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatPayload))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	gotKey := seenKey
	mu.Unlock()
	if gotKey != "byok-account-key" {
		t.Fatalf("upstream received key=%q, want byok-account-key (BYOK not used)", gotKey)
	}

	// cp must receive NO usage POST for BYOK-served requests.
	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt32(&cpHits); n != 0 {
		t.Fatalf("cp received %d usage POST(s) for BYOK request — must be 0", n)
	}
}

// TestBillingPathMultipleRequestsAllReachCP verifies that N sequential metered
// requests each result in a cp usage POST (no silent drops under normal conditions).
func TestBillingPathMultipleRequestsAllReachCP(t *testing.T) {
	const N = 5

	llmUp := mockLLM(1, 1)
	defer llmUp.Close()

	var mu sync.Mutex
	var posts int
	cpDone := make(chan struct{}, N)
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage" {
			mu.Lock()
			posts++
			mu.Unlock()
			select {
			case cpDone <- struct{}{}:
			default:
			}
		}
	}))
	defer cpSrv.Close()

	s := newE2EServer(t, llmUp.URL, NewUsageLogger(New(cpSrv.URL, "")))

	for i := 0; i < N; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatPayload))
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("request %d: status=%d", i, rec.Code)
		}
	}

	deadline := time.After(5 * time.Second)
	for i := 0; i < N; i++ {
		select {
		case <-cpDone:
		case <-deadline:
			mu.Lock()
			got := posts
			mu.Unlock()
			t.Fatalf("timed out: got %d/%d cp posts", got, N)
		}
	}

	mu.Lock()
	got := posts
	mu.Unlock()
	if got != N {
		t.Fatalf("cp posts=%d, want %d", got, N)
	}
}

// TestBillingPathCPOutageNoDropLocalRecord verifies that when cp is unreachable,
// the local (non-cp) usage logger still receives the record. Billing resilience
// requires that a cp outage never silently loses the local audit trail.
func TestBillingPathCPOutageNoDropLocalRecord(t *testing.T) {
	llmUp := mockLLM(10, 5)
	defer llmUp.Close()

	// cp unreachable.
	cpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cpURL := cpSrv.URL
	cpSrv.Close()

	cl := &countLogger{}
	cpLogger := NewUsageLogger(New(cpURL, ""))
	multi := NewMultiUsageLogger(cl, cpLogger)
	s := newE2EServer(t, llmUp.URL, multi)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatPayload))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("gateway status=%d body=%s", rec.Code, rec.Body.String())
	}

	// The local logger must receive its record synchronously, even though the cp
	// sink will fail asynchronously.
	if cl.n != 1 {
		t.Fatalf("cp outage: local logger got %d records, want 1", cl.n)
	}
}

// fakeByok is an in-test BYOKStore for the billing path tests.
type fakeByok struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

func (f *fakeByok) Get(a, p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.m[a][p]
	return k, ok
}
func (f *fakeByok) Set(a, p, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m[a] == nil {
		f.m[a] = map[string]string{}
	}
	f.m[a][p] = k
	return nil
}
func (f *fakeByok) Clear(a, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m[a], p)
	return nil
}
func (f *fakeByok) Providers(a string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.m[a] {
		out = append(out, k)
	}
	return out
}

package cp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/llmux/llmux/core/server"
)

// --- Identity ---------------------------------------------------------------

func TestIdentityResolve(t *testing.T) {
	var gotKey string
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get(HeaderRelayAuth)
		var body resolveRequest
		json.NewDecoder(r.Body).Decode(&body)
		gotKey = body.Key
		json.NewEncoder(w).Encode(resolveResponse{AccountID: "acct_99", Tier: "pro"})
	}))
	defer srv.Close()

	id := NewIdentity(New(srv.URL, "sekret"))
	p, ok := id.Resolve(context.Background(), "sk-token")
	if !ok {
		t.Fatal("resolve failed")
	}
	if p.AccountID != "acct_99" || p.Tier != "pro" {
		t.Fatalf("principal=%+v", p)
	}
	if p.Token != "sk-token" {
		t.Fatalf("token not preserved: %q", p.Token)
	}
	if gotPath != "/api/llm/resolve" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotKey != "sk-token" {
		t.Fatalf("cp received key=%q", gotKey)
	}
	if gotAuth != "sekret" {
		t.Fatalf("X-Relay-Auth=%q", gotAuth)
	}
}

func TestIdentityResolve404Unknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	id := NewIdentity(New(srv.URL, ""))
	if _, ok := id.Resolve(context.Background(), "sk-x"); ok {
		t.Fatal("404 should resolve to unknown (ok=false)")
	}
}

func TestIdentityResolveTransportError(t *testing.T) {
	// Point at a closed server so the request fails at transport.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	id := NewIdentity(New(url, ""))
	if _, ok := id.Resolve(context.Background(), "sk-x"); ok {
		t.Fatal("transport error must not admit the request")
	}
}

// --- BudgetGate -------------------------------------------------------------

func TestBudgetAllow(t *testing.T) {
	var gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get(HeaderRelayAuth)
		json.NewEncoder(w).Encode(entitlementResponse{LLMEnabled: true, LLMBudgetUSD: 12.5})
	}))
	defer srv.Close()

	g := NewBudgetGate(New(srv.URL, "topsecret"))
	d := g.Check(context.Background(), server.Principal{AccountID: "acct_1"})
	if d.Denied {
		t.Fatalf("expected allow, got deny: %q", d.Reason)
	}
	if gotQuery != "product=llm&account_id=acct_1" {
		t.Fatalf("query=%q", gotQuery)
	}
	if gotAuth != "topsecret" {
		t.Fatalf("X-Relay-Auth=%q", gotAuth)
	}
}

func TestBudgetDenials(t *testing.T) {
	cases := []struct {
		name string
		resp entitlementResponse
	}{
		{"disabled", entitlementResponse{LLMEnabled: false, LLMBudgetUSD: 10}},
		{"suspended", entitlementResponse{LLMEnabled: true, Suspended: true, LLMBudgetUSD: 10}},
		{"exhausted", entitlementResponse{LLMEnabled: true, LLMBudgetUSD: 0}},
		{"negative", entitlementResponse{LLMEnabled: true, LLMBudgetUSD: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(tc.resp)
			}))
			defer srv.Close()
			g := NewBudgetGate(New(srv.URL, ""))
			d := g.Check(context.Background(), server.Principal{AccountID: "acct_1"})
			if !d.Denied {
				t.Fatalf("%s: expected deny", tc.name)
			}
		})
	}
}

// TestBudgetColdCacheDegradedRPMBounds verifies the DEFAULT cold-cache posture:
// with cp unreachable and nothing cached, requests are allowed only up to a
// conservative per-account RPM cap and then rate-limited — not failed fully open
// (which would allow unbounded concurrency against real provider keys).
func TestBudgetColdCacheDegradedRPMBounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // transport error on every call: cp unreachable, cold cache

	const cap = 3
	g := NewBudgetGate(New(url, "").WithDegradedRPM(cap))

	allowed, limited := 0, 0
	for i := 0; i < cap+2; i++ {
		d := g.Check(context.Background(), server.Principal{AccountID: "acct_cold"})
		switch {
		case d.RateLimited:
			limited++
		case d.Denied:
			t.Fatalf("cold-cache request %d denied (should be allowed or rate-limited): %q", i, d.Reason)
		default:
			allowed++
		}
	}
	if allowed != cap {
		t.Fatalf("cold-cache allowed=%d, want exactly cap=%d", allowed, cap)
	}
	if limited != 2 {
		t.Fatalf("cold-cache rate-limited=%d, want 2 (over the cap)", limited)
	}
}

// TestBudgetColdCacheFailOpenOptIn verifies the operator opt-in: with
// DegradedFailOpen set, a cold-cache cp outage fails fully OPEN (historical
// behavior) with no bound.
func TestBudgetColdCacheFailOpenOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	g := NewBudgetGate(New(url, "").WithDegradedFailOpen(true).WithDegradedRPM(1))
	for i := 0; i < 10; i++ {
		d := g.Check(context.Background(), server.Principal{AccountID: "acct_open"})
		if d.Denied || d.RateLimited {
			t.Fatalf("fail-open opt-in: request %d should pass unbounded, got denied=%v limited=%v", i, d.Denied, d.RateLimited)
		}
	}
}

// TestBudgetWarmCacheSurvivesOutage verifies the last-known-good path still wins
// over degraded mode: a cached entitlement is enforced (with reservation) even
// after cp goes down, so degraded RPM only applies to a truly cold cache.
func TestBudgetWarmCacheSurvivesOutage(t *testing.T) {
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(entitlementResponse{LLMEnabled: true, LLMBudgetUSD: 100})
	}))
	defer srv.Close()

	g := NewBudgetGate(New(srv.URL, "").WithDegradedRPM(1))
	// Warm the cache.
	if d := g.Check(context.Background(), server.Principal{AccountID: "acct_warm"}); d.Denied {
		t.Fatalf("warm check denied: %q", d.Reason)
	}
	down = true
	// Even with cp down and degraded RPM=1, the cached budget keeps admitting.
	for i := 0; i < 5; i++ {
		d := g.Check(context.Background(), server.Principal{AccountID: "acct_warm"})
		if d.Denied || d.RateLimited {
			t.Fatalf("warm-cache request %d should use last-known-good, got denied=%v limited=%v", i, d.Denied, d.RateLimited)
		}
	}
}

func TestBudgetFailOpenServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	g := NewBudgetGate(New(srv.URL, ""))
	if d := g.Check(context.Background(), server.Principal{AccountID: "acct_1"}); d.Denied {
		t.Fatalf("cp 500 must fail OPEN, got deny: %q", d.Reason)
	}
}

// --- Usage ------------------------------------------------------------------

func TestUsagePostShape(t *testing.T) {
	var mu sync.Mutex
	var got usageBody
	var gotAuth, gotPath string
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get(HeaderRelayAuth)
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, "usagesecret"))
	u.Log(server.UsageRecord{AccountID: "acct_7", Total: 321, CostUSD: 0.0042})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("usage POST never arrived (fire-and-forget timed out)")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/usage" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotAuth != "usagesecret" {
		t.Fatalf("X-Relay-Auth=%q", gotAuth)
	}
	if got.Product != "llm" || got.Kind != "llm_tokens" {
		t.Fatalf("body product/kind=%+v", got)
	}
	if got.AccountID != "acct_7" || got.Count != 321 || got.CostUSD != 0.0042 {
		t.Fatalf("body=%+v", got)
	}
}

// TestUsageIdempotencyKey verifies the record id is sent both as the body
// idempotency_key and the Idempotency-Key header so cp can dedupe retries.
func TestUsageIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var got usageBody
	var hdr string
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hdr = r.Header.Get("Idempotency-Key")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, ""))
	u.Log(server.UsageRecord{ID: "usage-abc123", AccountID: "acct_7", Total: 10, CostUSD: 0.01})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("usage POST never arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	if got.IdempotencyKey != "usage-abc123" {
		t.Fatalf("body idempotency_key=%q, want usage-abc123", got.IdempotencyKey)
	}
	if hdr != "usage-abc123" {
		t.Fatalf("Idempotency-Key header=%q, want usage-abc123", hdr)
	}
}

func TestUsageSkipsNoAccount(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit <- struct{}{}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, ""))
	u.Log(server.UsageRecord{AccountID: "", Total: 5, CostUSD: 0.1})

	select {
	case <-hit:
		t.Fatal("record with no account id should not POST to cp")
	case <-time.After(300 * time.Millisecond):
		// expected: nothing sent
	}
}

// MultiUsageLogger fans out to all composed loggers.
func TestMultiUsageLoggerFanout(t *testing.T) {
	a := &countLogger{}
	b := &countLogger{}
	m := NewMultiUsageLogger(a, nil, b)
	m.Log(server.UsageRecord{AccountID: "x"})
	if a.n != 1 || b.n != 1 {
		t.Fatalf("fanout a=%d b=%d", a.n, b.n)
	}
}

type countLogger struct{ n int }

func (c *countLogger) Log(server.UsageRecord) { c.n++ }

// Enabled gates standalone vs cp wiring.
func TestEnabled(t *testing.T) {
	if New("", "x").Enabled() {
		t.Fatal("empty url should be disabled (standalone)")
	}
	if !New("https://cp.vulos.to", "").Enabled() {
		t.Fatal("set url should enable cp")
	}
	if New("https://cp.vulos.to/", "").BaseURL != "https://cp.vulos.to" {
		t.Fatal("trailing slash should be trimmed")
	}
}

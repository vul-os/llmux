package cp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
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

// TestIdentityLastKnownGoodOnOutage proves a brief cp outage degrades gracefully:
// a token that was SUCCESSFULLY resolved is reused (within TTL) when cp returns
// 5xx or is unreachable, while a token cp never confirmed is still refused.
func TestIdentityLastKnownGoodOnOutage(t *testing.T) {
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(resolveResponse{AccountID: "acct_7", Tier: "pro"})
	}))
	defer srv.Close()

	id := NewIdentity(New(srv.URL, ""))
	// Warm: a successful resolve caches the principal as last-known-good.
	if p, ok := id.Resolve(context.Background(), "sk-good"); !ok || p.AccountID != "acct_7" {
		t.Fatalf("warm resolve failed: %+v ok=%v", p, ok)
	}
	// cp now 5xx: the previously-confirmed token still resolves (fail-soft).
	down.Store(true)
	if p, ok := id.Resolve(context.Background(), "sk-good"); !ok || p.AccountID != "acct_7" {
		t.Fatalf("5xx outage should reuse last-known-good: %+v ok=%v", p, ok)
	}
	// A token never confirmed is NOT admitted during the outage (fail-closed).
	if _, ok := id.Resolve(context.Background(), "sk-never"); ok {
		t.Fatal("unconfirmed token must not be admitted during a cp outage")
	}
}

// TestIdentityRevokedEvictsCache proves a definitive cp rejection (4xx) is never
// overridden by the last-known-good cache: a revoked token stops working
// immediately and does not survive a subsequent cp outage.
func TestIdentityRevokedEvictsCache(t *testing.T) {
	var revoked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if revoked.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(resolveResponse{AccountID: "acct_7", Tier: "pro"})
	}))
	defer srv.Close()

	id := NewIdentity(New(srv.URL, ""))
	if _, ok := id.Resolve(context.Background(), "sk-tok"); !ok {
		t.Fatal("initial resolve should succeed")
	}
	// cp now revokes the token (404): definitive — must reject AND evict.
	revoked.Store(true)
	if _, ok := id.Resolve(context.Background(), "sk-tok"); ok {
		t.Fatal("revoked token (404) must be rejected, not served from cache")
	}
	// Even if cp then goes fully down, the evicted token is not resurrected.
	srv.Close()
	if _, ok := id.Resolve(context.Background(), "sk-tok"); ok {
		t.Fatal("evicted (revoked) token must not survive a later outage")
	}
}

// TestIdentityLastKnownGoodTTLBounded proves the last-known-good window is
// bounded: once the TTL lapses, an outage no longer admits the cached principal.
func TestIdentityLastKnownGoodTTLBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resolveResponse{AccountID: "acct_7", Tier: "pro"})
	}))

	id := NewIdentity(New(srv.URL, "").WithEntitlementTTL(20 * time.Millisecond))
	if _, ok := id.Resolve(context.Background(), "sk-tok"); !ok {
		t.Fatal("initial resolve should succeed")
	}
	// cp unreachable AND the cached principal has aged past the TTL.
	srv.Close()
	time.Sleep(40 * time.Millisecond)
	if _, ok := id.Resolve(context.Background(), "sk-tok"); ok {
		t.Fatal("expired last-known-good must not be admitted")
	}
}

// cacheLen reports the current size of the identity last-known-good cache.
func (i *Identity) cacheLen() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.cache)
}

// TestIdentityCachePrunesExpired proves the last-known-good cache does not grow
// unbounded: entries whose TTL has lapsed are swept on the next insert rather
// than lingering forever (one entry per distinct token, leaking memory).
func TestIdentityCachePrunesExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body resolveRequest
		json.NewDecoder(r.Body).Decode(&body)
		// Echo a distinct account per token so each is a real cache entry.
		json.NewEncoder(w).Encode(resolveResponse{AccountID: "acct_" + body.Key, Tier: "pro"})
	}))
	defer srv.Close()

	id := NewIdentity(New(srv.URL, "").WithEntitlementTTL(20 * time.Millisecond))
	// Populate the cache with several distinct tokens.
	for _, tok := range []string{"a", "b", "c", "d"} {
		if _, ok := id.Resolve(context.Background(), tok); !ok {
			t.Fatalf("resolve %q failed", tok)
		}
	}
	if got := id.cacheLen(); got != 4 {
		t.Fatalf("cache size after warmup = %d, want 4", got)
	}
	// Let every entry age past the TTL, then resolve one more token. The insert's
	// lazy sweep must drop the 4 expired entries, leaving only the fresh one.
	time.Sleep(40 * time.Millisecond)
	if _, ok := id.Resolve(context.Background(), "e"); !ok {
		t.Fatal("resolve \"e\" failed")
	}
	if got := id.cacheLen(); got != 1 {
		t.Fatalf("expired entries not pruned: cache size = %d, want 1", got)
	}
}

// TestIdentityCacheBoundedBySizeCap proves the cache stays bounded even when many
// distinct tokens are resolved within the TTL: past the configured cap the oldest
// entries are evicted, so the cache can never exceed the cap.
func TestIdentityCacheBoundedBySizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body resolveRequest
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(resolveResponse{AccountID: "acct_" + body.Key, Tier: "pro"})
	}))
	defer srv.Close()

	// Long TTL (entries stay fresh) but a tiny size cap: growth is bounded by the
	// cap via oldest-eviction, not by token count.
	id := NewIdentity(New(srv.URL, "").WithEntitlementTTL(time.Hour).WithIdentityCacheMax(3))
	for i := 0; i < 50; i++ {
		tok := "tok-" + strconv.Itoa(i)
		if _, ok := id.Resolve(context.Background(), tok); !ok {
			t.Fatalf("resolve %q failed", tok)
		}
		if got := id.cacheLen(); got > 3 {
			t.Fatalf("cache exceeded cap: size = %d after %d inserts, want <= 3", got, i+1)
		}
	}
	if got := id.cacheLen(); got != 3 {
		t.Fatalf("final cache size = %d, want exactly cap=3", got)
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

// TestUsageSkipsBYOK verifies BYOK usage is never billed to cp: a record marked
// BYOK is recorded locally by the core logger but the cp billing sink drops it.
func TestUsageSkipsBYOK(t *testing.T) {
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit <- struct{}{}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, ""))
	u.Log(server.UsageRecord{AccountID: "acct_7", Total: 5, CostUSD: 0.1, BYOK: true})

	select {
	case <-hit:
		t.Fatal("BYOK record must not POST to cp (unmetered)")
	case <-time.After(300 * time.Millisecond):
		// expected: nothing sent
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
	if !New("https://cp.vulos.org", "").Enabled() {
		t.Fatal("set url should enable cp")
	}
	if New("https://cp.vulos.org/", "").BaseURL != "https://cp.vulos.org" {
		t.Fatal("trailing slash should be trimmed")
	}
}

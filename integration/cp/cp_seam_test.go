package cp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vul-os/llmux/core/server"
)

// --- 4. Concurrent budget reservation blocks overspend ----------------------

// TestBudgetReservationBlocksConcurrent: with a tiny budget, only a bounded
// number of concurrent requests may pass before the reservation holds exhaust
// it. Without the reservation, all N would pass on the same near-zero balance.
func TestBudgetReservationBlocksConcurrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Budget just above one hold but below two: only 1 concurrent request fits.
		json.NewEncoder(w).Encode(entitlementResponse{LLMEnabled: true, LLMBudgetUSD: reservationHold * 1.5})
	}))
	defer srv.Close()

	g := NewBudgetGate(New(srv.URL, ""))
	p := server.Principal{AccountID: "acct_concurrent"}

	const n = 20
	var allowed int32
	var holds []server.BudgetDecision
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := g.Check(context.Background(), p)
			if d.Denied {
				return
			}
			atomic.AddInt32(&allowed, 1)
			mu.Lock()
			holds = append(holds, d) // hold the reservation (don't release yet)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// remaining = budget - inflight must never go <= 0 while holds are live, so
	// at most ceil(budget/hold) requests may be admitted concurrently. With
	// budget=1.5*hold that is 2 (the 2nd passes when remaining=1.5h-1h=0.5h>0,
	// the 3rd sees remaining=1.5h-2h<0 -> denied).
	if allowed == 0 {
		t.Fatal("no request admitted")
	}
	if allowed > 2 {
		t.Fatalf("reservation failed to bound concurrency: admitted %d on a %.3f budget (hold=%.3f)", allowed, reservationHold*1.5, reservationHold)
	}

	// Releasing frees the holds so a later request passes again.
	for _, d := range holds {
		if d.Release != nil {
			d.Release()
		}
	}
	if d := g.Check(context.Background(), p); d.Denied {
		t.Fatal("after releasing holds, a fresh request should be admitted")
	}
}

// --- cp-side RPM -------------------------------------------------------------

func TestBudgetRPMCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(entitlementResponse{LLMEnabled: true, LLMBudgetUSD: 1000})
	}))
	defer srv.Close()

	g := NewBudgetGate(New(srv.URL, "").WithRPM(3))
	p := server.Principal{AccountID: "acct_rpm"}

	var rl int
	for i := 0; i < 5; i++ {
		d := g.Check(context.Background(), p)
		if d.RateLimited {
			rl++
			continue
		}
		if d.Release != nil {
			d.Release()
		}
	}
	if rl != 2 {
		t.Fatalf("RPM=3 over 5 requests should rate-limit 2, got %d", rl)
	}
}

// --- 5. Usage-POST retry on transient failure -------------------------------

func TestUsageRetryOnTransientFailure(t *testing.T) {
	var calls int32
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 { // fail the first two, succeed on the third
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, ""))
	u.Log(server.UsageRecord{AccountID: "acct_retry", Total: 10, CostUSD: 0.01})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("usage POST never succeeded; calls=%d", atomic.LoadInt32(&calls))
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Fatalf("expected retries (>=3 calls), got %d", atomic.LoadInt32(&calls))
	}
}

func TestUsageGivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError) // always fail
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, ""))
	u.Log(server.UsageRecord{AccountID: "acct_giveup", Total: 1, CostUSD: 0.01})

	// Wait long enough for all attempts + backoff to elapse, then assert it
	// stopped at usageMaxAttempts (does not loop forever).
	time.Sleep(3 * time.Second)
	got := atomic.LoadInt32(&calls)
	if got != int32(usageMaxAttempts) {
		t.Fatalf("expected exactly %d attempts before giving up, got %d", usageMaxAttempts, got)
	}
}

// --- 6. Bounded last-known-good entitlement cache ---------------------------

func TestEntitlementCacheLastKnownGood(t *testing.T) {
	var serve int32 // 1 = serve a real entitlement, 0 = 500
	atomic.StoreInt32(&serve, 1)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if atomic.LoadInt32(&serve) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(entitlementResponse{LLMEnabled: true, LLMBudgetUSD: 100})
	}))
	defer srv.Close()

	// Short TTL so the second Check re-queries cp (which is now down).
	g := NewBudgetGate(New(srv.URL, "").WithEntitlementTTL(1 * time.Millisecond))
	p := server.Principal{AccountID: "acct_cache"}

	// 1. Warm the cache with a real entitlement (allowed).
	d := g.Check(context.Background(), p)
	if d.Denied {
		t.Fatalf("warm-up should be allowed: %q", d.Reason)
	}
	if d.Release != nil {
		d.Release()
	}

	// 2. cp goes down; let the TTL lapse so we re-query and fall back to cache.
	atomic.StoreInt32(&serve, 0)
	time.Sleep(5 * time.Millisecond)
	d = g.Check(context.Background(), p)
	if d.Denied {
		t.Fatalf("cp outage with warm cache must use last-known-good (allowed), got deny: %q", d.Reason)
	}
	if d.Release != nil {
		d.Release()
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Fatalf("expected a re-query after TTL, hits=%d", atomic.LoadInt32(&hits))
	}
}

func TestEntitlementColdCacheFailsOpen(t *testing.T) {
	// cp never reachable and nothing cached -> allow (degraded), not deny.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	g := NewBudgetGate(New(url, ""))
	d := g.Check(context.Background(), server.Principal{AccountID: "acct_cold"})
	if d.Denied {
		t.Fatalf("cold cache + cp outage must fail OPEN (degraded), got deny: %q", d.Reason)
	}
}

func TestEntitlementCachedDenialEnforcedDuringOutage(t *testing.T) {
	// A suspended account stays suspended even if cp later goes dark.
	var serve int32 = 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&serve) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(entitlementResponse{LLMEnabled: true, Suspended: true, LLMBudgetUSD: 100})
	}))
	defer srv.Close()

	g := NewBudgetGate(New(srv.URL, "").WithEntitlementTTL(1 * time.Millisecond))
	p := server.Principal{AccountID: "acct_susp"}

	if d := g.Check(context.Background(), p); !d.Denied {
		t.Fatal("suspended account should be denied while cp is up")
	}
	atomic.StoreInt32(&serve, 0)
	time.Sleep(5 * time.Millisecond)
	if d := g.Check(context.Background(), p); !d.Denied {
		t.Fatal("suspension must persist via last-known-good during cp outage")
	}
}

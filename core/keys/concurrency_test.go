package keys

import (
	"sync"
	"testing"
	"time"

	"github.com/llmux/llmux/core/config"
)

// TestMemLimiterRefillAndBurst covers the previously-untested token-bucket math.
func TestMemLimiterRefillAndBurst(t *testing.T) {
	lim := NewMemLimiter()
	base := time.Now()
	lim.now = func() time.Time { return base }

	// rpm=60 → capacity 60, refill 1/sec. Drain the full burst.
	for i := 0; i < 60; i++ {
		if !lim.Allow("k", 60) {
			t.Fatalf("burst token %d should be allowed", i)
		}
	}
	if lim.Allow("k", 60) {
		t.Fatal("61st request should be denied (bucket empty)")
	}
	// Advance 5s → exactly 5 tokens refill.
	lim.now = func() time.Time { return base.Add(5 * time.Second) }
	for i := 0; i < 5; i++ {
		if !lim.Allow("k", 60) {
			t.Fatalf("refilled token %d should be allowed", i)
		}
	}
	if lim.Allow("k", 60) {
		t.Fatal("6th after 5s refill should be denied")
	}
	// Large jump refills only up to capacity (no overflow).
	lim.now = func() time.Time { return base.Add(time.Hour) }
	allowed := 0
	for i := 0; i < 200; i++ {
		if lim.Allow("k", 60) {
			allowed++
		}
	}
	if allowed != 60 {
		t.Fatalf("after 1h, refill capped at capacity: allowed=%d, want 60", allowed)
	}
}

// TestConcurrentSpendNoLostUpdates runs under -race: concurrent AddSpend must
// sum exactly (micro-dollar integer accounting).
func TestConcurrentSpendNoLostUpdates(t *testing.T) {
	s := newMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 0}})
	var wg sync.WaitGroup
	const G, N = 50, 100
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < N; i++ {
				s.AddSpend("k", 0.01)
			}
		}()
	}
	wg.Wait()
	got := s.Spend("k")
	want := float64(G*N) * 0.01 // 50 USD, exact in micro-dollars
	if got != want {
		t.Fatalf("spend=%v, want %v (lost updates / float drift)", got, want)
	}
}

func TestBudgetExactThreshold(t *testing.T) {
	s := newMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 1.0}})
	s.AddSpend("k", 0.999999)
	if s.OverBudget("k") {
		t.Fatal("0.999999 < 1.0 should be under budget")
	}
	s.AddSpend("k", 0.000001) // exactly 1.0
	if !s.OverBudget("k") {
		t.Fatal("spend == budget must be over (>=)")
	}
}

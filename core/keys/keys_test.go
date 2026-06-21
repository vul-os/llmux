package keys

import (
	"sync"
	"testing"

	"github.com/llmux/llmux/core/config"
)

func TestBudget(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 1.0}})
	if s.OverBudget("k") {
		t.Fatal("should start under budget")
	}
	s.AddSpend("k", 0.5)
	if s.OverBudget("k") {
		t.Fatal("0.5 < 1.0 should be under budget")
	}
	s.AddSpend("k", 0.6)
	if !s.OverBudget("k") {
		t.Fatal("1.1 >= 1.0 should be over budget")
	}
	if s.Spend("k") != 1.1 {
		t.Fatalf("spend=%v", s.Spend("k"))
	}
}

func TestUnlimitedBudget(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 0}})
	s.AddSpend("k", 1000)
	if s.OverBudget("k") {
		t.Fatal("budget 0 means unlimited")
	}
}

func TestRateLimitAllow(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{{Key: "k", RPM: 2}})
	if !s.Allow("k") || !s.Allow("k") {
		t.Fatal("first two requests should pass with rpm=2")
	}
	if s.Allow("k") {
		t.Fatal("third request should be limited")
	}
}

// TestMemStoreConcurrentSpendAndAllow exercises the lock-free hot path under
// -race: many goroutines hammer AddSpend, Allow, and Lookup concurrently. The
// run must be race-free and the spend total must be exact (integer micro-dollar
// accounting, no lost updates).
func TestMemStoreConcurrentSpendAndAllow(t *testing.T) {
	s := newMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 0, RPM: 1000}})
	var wg sync.WaitGroup
	const G, N = 64, 200
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < N; i++ {
				s.AddSpend("k", 0.01)
				_ = s.Allow("k")
				if k, ok := s.Lookup("k"); !ok || k.Key != "k" {
					t.Errorf("lookup failed during concurrency")
					return
				}
				_ = s.OverBudget("k")
				_ = s.Spend("k")
			}
		}()
	}
	wg.Wait()
	got := s.Spend("k")
	want := float64(G*N) * 0.01 // exact in micro-dollars
	if got != want {
		t.Fatalf("spend=%v, want %v (lost updates / torn read)", got, want)
	}
}

func TestAllowsModel(t *testing.T) {
	k := &Key{AllowedModels: []string{"gpt-4o"}}
	if !k.AllowsModel("gpt-4o") || k.AllowsModel("other") {
		t.Fatal("allow-list mismatch")
	}
	open := &Key{}
	if !open.AllowsModel("anything") {
		t.Fatal("empty allow-list means all models allowed")
	}
}

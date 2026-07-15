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

// TestSubCentSpendAccountingNoDrift is a billing-accuracy boundary: spend is
// tracked in integer micro-dollars precisely so that thousands of sub-cent
// charges sum EXACTLY, with none of the float64 accumulation drift that would
// slowly over- or under-bill a busy key. 10_000 charges of $0.0001 must total
// exactly $1.00.
func TestSubCentSpendAccountingNoDrift(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 0}})
	const n = 10_000
	for i := 0; i < n; i++ {
		s.AddSpend("k", 0.0001)
	}
	if got := s.Spend("k"); got != 1.0 {
		t.Fatalf("sub-cent accounting drifted: got %.10f, want exactly 1.0", got)
	}
}

// TestOverBudgetBoundaryExact pins the budget-gate boundary: OverBudget trips at
// spend >= budget (not strictly greater), so a key that has spent EXACTLY its
// budget is denied the next request. One micro-dollar under must still pass.
func TestOverBudgetBoundaryExact(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 2.0}})
	s.AddSpend("k", 1.999999) // one micro-dollar under the cap
	if s.OverBudget("k") {
		t.Fatalf("spend just under budget must be allowed: spend=%v", s.Spend("k"))
	}
	s.AddSpend("k", 0.000001) // now exactly at the cap
	if !s.OverBudget("k") {
		t.Fatalf("spend exactly at budget must be over: spend=%v", s.Spend("k"))
	}
}

// TestOverBudgetUnknownKeyIsNotOver: an unknown token has no configured budget,
// so the in-memory gate reports it as not-over (the authenticated path already
// rejected unknown tokens upstream; OverBudget must not panic or misreport).
func TestOverBudgetUnknownKeyIsNotOver(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{{Key: "k", BudgetUSD: 1.0}})
	if s.OverBudget("no-such-token") {
		t.Fatal("unknown token must not report over budget")
	}
	if got := s.Spend("no-such-token"); got != 0 {
		t.Fatalf("unknown token spend = %v, want 0", got)
	}
	// AddSpend to an unknown token is a no-op (never allocates a phantom counter).
	s.AddSpend("no-such-token", 5)
	if got := s.Spend("no-such-token"); got != 0 {
		t.Fatalf("AddSpend to unknown token must be a no-op, got %v", got)
	}
}

// TestKeysListing verifies the admin Keys() listing returns every configured key.
func TestKeysListing(t *testing.T) {
	s := NewMemStore([]config.KeyConfig{
		{Key: "k1", Name: "one"},
		{Key: "k2", Name: "two"},
	})
	got := s.Keys()
	if len(got) != 2 {
		t.Fatalf("Keys() len = %d, want 2", len(got))
	}
	names := map[string]bool{}
	for _, k := range got {
		names[k.Name] = true
	}
	if !names["one"] || !names["two"] {
		t.Fatalf("Keys() missing entries: %v", names)
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

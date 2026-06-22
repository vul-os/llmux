package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/keys"
)

// keyedServer builds a server with one static key configured.
func keyedServer(t *testing.T, up *httptest.Server, key config.KeyConfig) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:   []config.KeyConfig{key},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestStandaloneIdentityUnchanged verifies the default (no-cp) path uses the
// static key store: account id == key name, valid key passes, bad key 401s.
func TestStandaloneIdentityUnchanged(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := keyedServer(t, up, config.KeyConfig{Key: "sk-good", Name: "alice"})

	// The default Identity is the static one.
	if _, ok := s.identity.(staticIdentity); !ok {
		t.Fatalf("default identity = %T, want staticIdentity", s.identity)
	}
	if _, ok := s.budget.(*staticBudgetGate); !ok {
		t.Fatalf("default budget = %T, want *staticBudgetGate", s.budget)
	}

	p, ok := s.identity.Resolve(context.Background(), "sk-good")
	if !ok {
		t.Fatal("valid key did not resolve")
	}
	if p.AccountID != "alice" {
		t.Fatalf("account id = %q, want key name 'alice'", p.AccountID)
	}
	if _, ok := s.identity.Resolve(context.Background(), "sk-bad"); ok {
		t.Fatal("unknown key resolved")
	}

	// End-to-end: good key gets 200, bad key gets 401.
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	for _, tc := range []struct {
		token string
		want  int
	}{{"sk-good", 200}, {"sk-bad", 401}} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("token %q: status=%d want %d body=%s", tc.token, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// TestStandaloneBudgetDeny verifies the static budget gate denies (402) once a
// key is over budget — the original behavior, now via the BudgetGate seam.
func TestStandaloneBudgetDeny(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	s := keyedServer(t, up, config.KeyConfig{Key: "sk-tight", Name: "bob", BudgetUSD: 0.0001})

	// Push spend over the tiny budget.
	s.keys.AddSpend("sk-tight", 1.0)
	if d := s.budget.Check(context.Background(), Principal{Token: "sk-tight", AccountID: "bob"}); !d.Denied {
		t.Fatal("expected over-budget deny")
	}

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-tight")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 402 {
		t.Fatalf("status=%d want 402 body=%s", rec.Code, rec.Body.String())
	}
}

// TestStaticBudgetReservationBoundsConcurrency verifies the static gate places
// an in-flight reservation so that many concurrent requests near the budget
// limit can't ALL pass the OverBudget check before any has recorded spend (which
// previously let them overshoot BudgetUSD). With budget=0.10 and a per-request
// hold of staticReservationHold (0.05), at most ceil(0.10/0.05)=2 requests can
// be admitted while none has yet recorded spend; the rest are denied.
func TestStaticBudgetReservationBoundsConcurrency(t *testing.T) {
	store := keys.NewMemStore([]config.KeyConfig{{Key: "sk", Name: "team", BudgetUSD: 0.10}})
	g := newStaticBudgetGate(store)
	p := Principal{Token: "sk", AccountID: "team"}
	if k, ok := store.Lookup("sk"); ok {
		p.Key = k
	}

	const n = 50
	var allowed, denied int64
	var wg sync.WaitGroup
	var holds []func()
	var hmu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := g.Check(context.Background(), p)
			if d.Denied {
				atomic.AddInt64(&denied, 1)
				return
			}
			atomic.AddInt64(&allowed, 1)
			// Hold the reservation (don't release) to model in-flight requests
			// that have not yet recorded spend.
			if d.Release != nil {
				hmu.Lock()
				holds = append(holds, d.Release)
				hmu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Without the reservation layer, all 50 would pass (spend is still 0). With
	// it, the in-flight holds bound concurrent admits to the budget/hold ratio.
	maxAllowed := int64(0.10/staticReservationHold) + 1 // small slack for float edge
	if allowed > maxAllowed {
		t.Fatalf("allowed=%d exceeds reservation bound (~%d); reservation not enforced", allowed, maxAllowed)
	}
	if allowed == n {
		t.Fatalf("all %d concurrent requests passed — reservation absent (would overshoot budget)", n)
	}
	if allowed+denied != n {
		t.Fatalf("allowed(%d)+denied(%d) != %d", allowed, denied, n)
	}

	// Releasing the holds frees budget again for subsequent requests.
	for _, r := range holds {
		r()
	}
	if d := g.Check(context.Background(), p); d.Denied {
		t.Fatalf("after releasing holds, a fresh request should pass (spend still under budget): %q", d.Reason)
	}
}

// stubIdentity / stubBudget exercise the seam override + account-id propagation
// without any cp dependency (the core test must not import integration/cp).
type stubIdentity struct {
	account string
	ok      bool
}

func (i stubIdentity) Resolve(_ context.Context, token string) (Principal, bool) {
	if !i.ok {
		return Principal{}, false
	}
	return Principal{Token: token, AccountID: i.account, Tier: "test"}, true
}

type stubBudget struct{ deny bool }

func (b stubBudget) Check(_ context.Context, _ Principal) BudgetDecision {
	if b.deny {
		return BudgetDecision{Denied: true, Reason: "stub deny"}
	}
	return BudgetDecision{}
}

// TestExternalIdentityAccountPropagation verifies an injected Identity activates
// the auth path even with no static keys, and the resolved account id flows into
// the usage record (so cost attributes to the Vulos account).
func TestExternalIdentityAccountPropagation(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	// No static keys configured — cp is the source of truth.
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &captureLogger{}
	s.SetIdentity(stubIdentity{account: "acct_42", ok: true})
	s.SetBudgetGate(stubBudget{})
	s.SetUsageLogger(rec)

	if !s.identityActive() {
		t.Fatal("external identity should activate auth path")
	}

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.recs) == 0 {
		t.Fatal("no usage record emitted")
	}
	if rec.recs[0].AccountID != "acct_42" {
		t.Fatalf("usage account id = %q, want acct_42", rec.recs[0].AccountID)
	}
}

// TestExternalBudgetDeny verifies an injected BudgetGate deny produces 402.
func TestExternalBudgetDeny(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, _ := New(cfg)
	s.SetIdentity(stubIdentity{account: "acct_42", ok: true})
	s.SetBudgetGate(stubBudget{deny: true})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 402 {
		t.Fatalf("status=%d want 402 body=%s", w.Code, w.Body.String())
	}
}

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vul-os/llmux/core/config"
)

// Gateway.Authorize hands the caller a release func and documents that it must
// be called exactly once, whatever the outcome. It is how an in-flight budget
// reservation is given back (see TestStaticBudgetReservationBoundsConcurrency:
// unreleased holds bound admissions, so a leaked one permanently shrinks the
// account's usable budget until the process restarts).
//
// Nothing asserted that the HTTP path actually calls it. A `defer release()`
// dropped in a refactor compiles, serves every request correctly, and only
// shows up later as an account that mysteriously cannot spend its budget.
//
// countingGate hands out a distinct Release for every admitted request and
// counts both ends.
type countingGate struct {
	deny     bool
	checks   atomic.Int64
	releases atomic.Int64
}

func (g *countingGate) Check(_ context.Context, _ Principal) BudgetDecision {
	g.checks.Add(1)
	if g.deny {
		return BudgetDecision{Denied: true, Reason: "counting gate deny", Release: func() { g.releases.Add(1) }}
	}
	return BudgetDecision{Release: func() { g.releases.Add(1) }}
}

func releaseTestServer(t *testing.T, up *httptest.Server, gate *countingGate) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "test-key"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:   []config.KeyConfig{{Key: "sk-release", Name: "release"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBudgetGate(gate)
	if !s.identityActive() {
		t.Fatal("auth path is not active — this test would never reach Authorize")
	}
	return s
}

// TestHTTPPathAlwaysReleasesTheBudgetHold drives real requests through the
// server's handler and asserts the reservation is given back on every outcome.
func TestHTTPPathAlwaysReleasesTheBudgetHold(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()

	cases := []struct {
		name       string
		deny       bool
		req        func() *http.Request
		wantStatus int
	}{
		{
			name: "served request",
			req: func() *http.Request {
				r := httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set("Authorization", "Bearer sk-release")
				return r
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "request the gate denies",
			deny: true,
			req: func() *http.Request {
				r := httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set("Authorization", "Bearer sk-release")
				return r
			},
			wantStatus: http.StatusPaymentRequired,
		},
		{
			name: "request that fails after admission (no route for the model)",
			req: func() *http.Request {
				r := httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set("Authorization", "Bearer sk-release")
				return r
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &countingGate{deny: tc.deny}
			s := releaseTestServer(t, up, gate)

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, tc.req())
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want %d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}

			// The premise: the gate was actually consulted. Without this a
			// route that never authenticates would satisfy 0 == 0 below.
			if got := gate.checks.Load(); got != 1 {
				t.Fatalf("budget gate consulted %d times, want 1 — this request never reached Authorize, "+
					"so the release assertion below would be vacuous", got)
			}
			if got := gate.releases.Load(); got != 1 {
				t.Fatalf("the budget hold was released %d times, want exactly 1. An unreleased reservation "+
					"permanently shrinks the account's usable budget (see StaticBudgetGate); releasing twice "+
					"hands back budget that was never held.", got)
			}
		})
	}
}

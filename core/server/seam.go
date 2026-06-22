package server

import (
	"context"
	"sync"

	"github.com/llmux/llmux/core/keys"
)

// This file defines the integration "seam" between the llmux core gateway and
// any external control plane ("cp" / vulos-cloud).
//
// GOAL: llmux must run COMPLETELY STANDALONE with NO dependency on cp. The
// standalone path is the default and works with zero cp configuration — it is
// exactly the original static-key behavior (keys.Lookup / keys.OverBudget).
//
// The seam is two small interfaces that authMW calls instead of reaching into
// keys.* directly:
//
//   - Identity:   resolve a bearer token -> a Vulos account id + tier.
//   - BudgetGate: gate a request by the resolved account's LLM budget.
//
// Two kinds of implementation satisfy them:
//
//   - STANDALONE DEFAULTS (this package, staticIdentity / staticBudgetGate):
//     wrap the existing keys.Store. account id = key name. No cp, no network.
//
//   - CP ADAPTER (integration/cp, a SEPARATE package): implements the same
//     interfaces against the control plane. The core MUST NOT import it; only
//     the composition root (cmd/llmux/main.go) wires it, and only when
//     LLMUX_CP_URL is set. Deleting integration/cp never breaks the core build.

// Principal is the resolved identity of an authenticated request.
type Principal struct {
	// Token is the raw bearer token the caller presented.
	Token string
	// AccountID is the canonical Vulos account id. In standalone mode this is
	// the static key's name; with cp it is the account id cp resolved.
	AccountID string
	// Tier is a free-form tier label ("static", "free", "pro", ...).
	Tier string
	// Key is the resolved static key, when one applies (standalone mode). It is
	// nil for cp-resolved principals that have no local key. Handlers that need
	// per-key model allow-lists / spend recording read this.
	Key *keys.Key
}

// Identity resolves a bearer token to a Vulos account.
type Identity interface {
	// Resolve maps token to a Principal. ok is false when the token is unknown
	// (the caller returns 401). Implementations must not block indefinitely.
	Resolve(ctx context.Context, token string) (Principal, bool)
}

// BudgetDecision is the outcome of a budget/entitlement check.
type BudgetDecision struct {
	// Denied is true when the request must be rejected (HTTP 402). It is only
	// ever set on an EXPLICIT deny from the authority (static over-budget, or a
	// cp answer of !llm_enabled || suspended || remaining<=0).
	Denied bool
	// RateLimited is true when the request is rejected for exceeding the
	// principal's request-rate cap (HTTP 429), not budget. Mutually exclusive
	// with Denied in practice; checked first by the server.
	RateLimited bool
	// Reason is a short human label for the denial (used in the error body).
	Reason string
	// Release, when non-nil, frees any reservation/hold the gate placed for an
	// ALLOWED request. The server calls it exactly once after the request
	// completes (whatever the outcome). Gates that hold no reservation leave it
	// nil. It must be safe to call on a zero value via releaseDecision.
	Release func()
}

// BudgetGate gates a request by the principal's LLM budget / entitlements.
//
// Implementations MUST fail open on transport errors talking to an external
// authority: a cp blip must never hard-down the gateway. Only an explicit deny
// (over budget / suspended / disabled) sets Denied.
type BudgetGate interface {
	// Check evaluates p's budget. A zero BudgetDecision means "allowed".
	Check(ctx context.Context, p Principal) BudgetDecision
}

// ---------------------------------------------------------------------------
// Standalone defaults (wrap the existing keys.Store; no cp, no network).
// ---------------------------------------------------------------------------

// staticIdentity resolves tokens against the configured static key store. The
// account id is the key's name — the original behavior.
type staticIdentity struct{ keys keys.Store }

// Resolve implements Identity using keys.Lookup.
func (s staticIdentity) Resolve(_ context.Context, token string) (Principal, bool) {
	k, ok := s.keys.Lookup(token)
	if !ok {
		return Principal{}, false
	}
	return Principal{Token: token, AccountID: k.Name, Tier: "static", Key: k}, true
}

// staticReservationHold is the per-request in-flight cost reserved against a
// static key's budget while a request is outstanding. Like the cp gate, the real
// cost isn't known until the request finishes, so a nominal hold bounds how far
// concurrent requests can overshoot a near-exhausted budget: at most
// (in-flight requests x staticReservationHold) USD above the configured budget.
const staticReservationHold = 0.05

// staticBudgetGate gates by the static per-key budget (keys.OverBudget). It adds
// an in-flight reservation layer (mirroring the cp gate) so that N concurrent
// requests near the budget limit can't all pass the OverBudget check before any
// of them has recorded spend, and overshoot the BudgetUSD cap.
type staticBudgetGate struct {
	keys keys.Store

	mu       sync.Mutex
	inflight map[string]float64 // key token -> reserved in-flight USD
}

// newStaticBudgetGate builds the static gate with its reservation map.
func newStaticBudgetGate(store keys.Store) *staticBudgetGate {
	return &staticBudgetGate{keys: store, inflight: map[string]float64{}}
}

// Check implements BudgetGate using keys.OverBudget plus an in-flight reservation
// so concurrent requests can't collectively overshoot the key's budget.
func (s *staticBudgetGate) Check(_ context.Context, p Principal) BudgetDecision {
	name := p.AccountID
	if p.Key != nil {
		name = p.Key.Name
	}
	// Hard deny: spend already at/over the configured budget.
	if s.keys.OverBudget(p.Token) {
		return BudgetDecision{Denied: true, Reason: "budget exceeded for key " + name}
	}
	// Unlimited budget (BudgetUSD<=0) needs no reservation: nothing to overshoot.
	budget := budgetForKey(p)
	if budget <= 0 {
		return BudgetDecision{}
	}
	// Reservation: deny when recorded spend + outstanding in-flight holds would
	// reach the budget, else place a hold and release it on completion.
	remaining := budget - s.keys.Spend(p.Token)
	s.mu.Lock()
	remaining -= s.inflight[p.Token]
	if remaining <= 0 {
		s.mu.Unlock()
		return BudgetDecision{Denied: true, Reason: "budget exceeded for key " + name}
	}
	s.inflight[p.Token] += staticReservationHold
	s.mu.Unlock()

	token := p.Token
	return BudgetDecision{Release: func() { s.releaseStatic(token) }}
}

// releaseStatic frees one request's reservation hold for a key token.
func (s *staticBudgetGate) releaseStatic(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.inflight[token] - staticReservationHold
	if v <= 0 {
		delete(s.inflight, token)
		return
	}
	s.inflight[token] = v
}

// budgetForKey returns the configured BudgetUSD for the principal's key, or 0
// (unlimited) when no key applies.
func budgetForKey(p Principal) float64 {
	if p.Key != nil {
		return p.Key.BudgetUSD
	}
	return 0
}

// releaseDecision frees any reservation a gate placed for an allowed request.
// nil-safe so the static gate (which holds nothing) needs no Release.
func releaseDecision(d BudgetDecision) {
	if d.Release != nil {
		d.Release()
	}
}

// SetIdentity overrides the request-identity resolver (e.g. with the cp
// adapter). nil is ignored so the static default stays in place. Setting a
// non-default Identity also activates the authenticated path even when no
// static keys are configured (cp is the source of truth).
func (s *Server) SetIdentity(id Identity) {
	if id != nil {
		s.identity = id
		s.externalIdentity = true
	}
}

// identityActive reports whether the authenticated (Identity/BudgetGate) path
// should run. It runs when static keys are configured (original behavior) OR
// when an external Identity (e.g. cp) has been wired in.
func (s *Server) identityActive() bool {
	return len(s.cfg.Keys) > 0 || s.externalIdentity
}

// SetBudgetGate overrides the budget gate (e.g. with the cp adapter). nil is
// ignored so the static default stays in place.
func (s *Server) SetBudgetGate(g BudgetGate) {
	if g != nil {
		s.budget = g
	}
}

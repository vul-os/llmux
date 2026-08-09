package server

import "github.com/vul-os/llmux/core/gateway"

// The integration seam between llmux and an external control plane ("cp") lives
// in core/gateway, because an embedding host wires it too. These aliases keep
// the names cmd/llmux and integration/cp already use.
//
// GOAL (unchanged): llmux runs COMPLETELY STANDALONE with NO dependency on cp.
// The standalone path is the default and works with zero cp configuration — the
// original static-key behavior (keys.Lookup / keys.OverBudget).

// Principal is the resolved identity of an authenticated request.
type Principal = gateway.Principal

// Identity resolves a bearer token to a Vulos account.
type Identity = gateway.Identity

// BudgetDecision is the outcome of a budget/entitlement check.
type BudgetDecision = gateway.BudgetDecision

// BudgetGate gates a request by the principal's LLM budget / entitlements.
type BudgetGate = gateway.BudgetGate

// SetIdentity overrides the request-identity resolver (e.g. with the cp
// adapter). nil is ignored so the static default stays in place.
func (s *Server) SetIdentity(id Identity) { s.gw.SetIdentity(id) }

// SetBudgetGate overrides the budget gate (e.g. with the cp adapter). nil is
// ignored so the static default stays in place.
func (s *Server) SetBudgetGate(g BudgetGate) { s.gw.SetBudgetGate(g) }

// identityActive reports whether the authenticated (Identity/BudgetGate) path
// should run.
func (s *Server) identityActive() bool { return s.gw.IdentityActive() }

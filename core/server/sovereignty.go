package server

import (
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/sovereign"
)

// enforceSovereignty is the dispatch-time gate that makes "nothing leaves the
// box unless you say so" a real, enforced property. The check itself lives in
// core/gateway (which owns the policy and the metrics counter); this is the
// shell's handle on it, used by the modality and transcription forwards that
// still dispatch from this package.
//
// core/sovereign/egress_guard_test.go requires the gate to be called, by this
// exact name, in the SAME function as every provider dispatch — including the
// forwards below. Renaming this fails that guard loudly, which is the point.
func (s *Server) enforceSovereignty(provName string) error {
	return s.gw.EnforceSovereignty(provName)
}

// Sovereign returns the gateway's sovereignty policy (posture reporting).
func (s *Server) Sovereign() *sovereign.Policy { return s.gw.Sovereign() }

// SetSovereignPolicy replaces the sovereignty policy. nil is ignored so the
// config-derived default can never be silently disarmed.
func (s *Server) SetSovereignPolicy(p *sovereign.Policy) { s.gw.SetSovereignPolicy(p) }

var _ = gateway.AsProviderError

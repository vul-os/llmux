package server

import (
	"log"
	"net/http"

	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
	"github.com/llmux/llmux/core/sovereign"
)

// enforceSovereignty is the dispatch-time gate that makes "nothing leaves the
// box unless you say so" a real, enforced property. It returns nil when a
// request may be sent to provName, or a 403 provider.Error when provName is a
// non-local endpoint the operator has not explicitly opted in (allow_egress).
//
// The check happens BEFORE any network call, so a denied provider never even
// opens a connection. Permitted egress is logged/labeled on every request so
// off-box traffic is always observable, never silent.
func (s *Server) enforceSovereignty(provName string) error {
	d := s.sovereign.Check(provName)
	if !d.Allowed {
		s.metrics.incEgressBlocked()
		s.log.Warn("sovereignty: blocked egress",
			"provider", provName, "tier", string(d.Tier), "label", d.Label(),
			"locality", string(d.Locality), "base_url", d.BaseURL)
		return &provider.Error{
			StatusCode: http.StatusForbidden,
			Provider:   provName,
			Body: openai.NewError(
				"sovereignty: provider \""+provName+"\" is a non-local endpoint and egress is not enabled; "+
					"set \"allow_egress\": true on this provider to permit off-box calls",
				"sovereignty_error", "egress_not_allowed"),
		}
	}
	if d.Tier != sovereign.TierLocal {
		// Permitted but off-box: label every such call with its tier so
		// sovereign/brokered/external traffic is always observable, never silent.
		s.log.Info("sovereignty: off-box call permitted",
			"provider", provName, "tier", string(d.Tier), "label", d.Label(),
			"locality", string(d.Locality), "base_url", d.BaseURL)
	}
	return nil
}

// logSovereignty prints the sovereignty posture at startup so operators can see
// exactly which tier each provider runs in and which off-box tiers are permitted.
func logSovereignty(p *sovereign.Policy) {
	var local, sovereignT, brokeredOK, externalOK, blocked []string
	for _, d := range p.Decisions() {
		switch {
		case d.Tier == sovereign.TierLocal:
			local = append(local, d.Provider)
		case d.Tier == sovereign.TierSovereign:
			sovereignT = append(sovereignT, d.Provider)
		case !d.Allowed:
			blocked = append(blocked, d.Provider)
		case d.Tier == sovereign.TierBrokered:
			brokeredOK = append(brokeredOK, d.Provider)
		default: // external, allowed via allow_egress
			externalOK = append(externalOK, d.Provider)
		}
	}
	log.Printf("llmux sovereignty (where your AI runs): local=%v sovereign=%v brokered-allowed=%v external-allowed=%v blocked=%v",
		local, sovereignT, brokeredOK, externalOK, blocked)
	if len(blocked) > 0 {
		log.Printf("llmux sovereignty: %d off-box provider(s) BLOCKED by default; "+
			"mark a trusted pool \"tier\": \"sovereign\", opt a broker in with \"allow_brokered\": true, "+
			"or set \"allow_egress\": true to permit external off-box calls", len(blocked))
	}
}

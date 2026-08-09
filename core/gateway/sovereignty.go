package gateway

import (
	"log/slog"
	"net/http"

	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/provider"
	"github.com/vul-os/llmux/core/sovereign"
)

// enforceSovereignty is the dispatch-time gate that makes "nothing leaves the
// box unless you say so" a real, enforced property. It returns nil when a
// request may be sent to provName, or a 403 provider.Error when provName is a
// non-local endpoint the operator has not explicitly opted in (allow_egress).
//
// The check happens BEFORE any network call, so a denied provider never even
// opens a connection. Permitted egress is logged/labeled on every request so
// off-box traffic is always observable, never silent.
//
// core/sovereign/egress_guard_test.go requires this exact name in the SAME
// function as every provider dispatch, so the shell's delegating wrapper
// (core/server/sovereignty.go) keeps the name too.
func (g *Gateway) enforceSovereignty(provName string) error {
	d := g.sovereign.Check(provName)
	if !d.Allowed {
		g.metrics.incEgressBlocked()
		g.log.Warn("sovereignty: blocked egress",
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
		g.log.Info("sovereignty: off-box call permitted",
			"provider", provName, "tier", string(d.Tier), "label", d.Label(),
			"locality", string(d.Locality), "base_url", d.BaseURL)
	}
	return nil
}

// EnforceSovereignty is the exported gate, for dispatch paths that live outside
// this package (the HTTP shell's modality/transcription forwards). It is the
// same check the gateway's own dispatch runs.
func (g *Gateway) EnforceSovereignty(provName string) error { return g.enforceSovereignty(provName) }

// Sovereign returns the sovereignty policy (posture reporting).
func (g *Gateway) Sovereign() *sovereign.Policy { return g.sovereign }

// SetSovereignPolicy replaces the sovereignty policy. It exists for tests and
// for hosts that compute the policy themselves; nil is ignored so the
// config-derived default can never be silently disarmed.
func (g *Gateway) SetSovereignPolicy(p *sovereign.Policy) {
	if p != nil {
		g.sovereign = p
	}
}

// logSovereignty records the sovereignty posture at startup so operators can see
// exactly which tier each provider runs in and which off-box tiers are permitted.
func logSovereignty(log *slog.Logger, p *sovereign.Policy) {
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
	log.Info("sovereignty posture (where your AI runs)",
		"local", local, "sovereign", sovereignT,
		"brokered_allowed", brokeredOK, "external_allowed", externalOK, "blocked", blocked)
	if len(blocked) > 0 {
		log.Warn("sovereignty: off-box providers BLOCKED by default; "+
			"mark a trusted pool \"tier\": \"sovereign\", opt a broker in with \"allow_brokered\": true, "+
			"or set \"allow_egress\": true to permit external off-box calls",
			"blocked_count", len(blocked))
	}
}

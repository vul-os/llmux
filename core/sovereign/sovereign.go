// Package sovereign enforces llmux's sovereignty guarantee: "you choose WHERE
// your AI runs, and it is never silently sent to a company that mines you." The
// choice is a 4-tier dial (most→least private):
//
//	local     — inference on THIS box (loopback / unix socket). Always allowed.
//	sovereign — a Vulos-operated trusted endpoint (in-region, no-train, isolated),
//	            declared by the operator. Allowed BY DEFAULT: it is inside the
//	            sovereignty boundary by operator declaration.
//	brokered  — a named third party under a no-train agreement, operator-configured.
//	            Allowed ONLY when the operator opts in.
//	external  — any other off-box endpoint (may mine/train). BLOCKED unless the
//	            operator sets the allow_egress escape hatch.
//
// Nothing silently upgrades: sovereign/brokered are EXPLICIT operator config
// declarations; an unmarked endpoint derives its tier from locality (loopback→
// local, else external). It fails CLOSED: anything unknown/unclassifiable is
// treated as external and blocked. Every permitted call is logged/labeled with
// its tier so off-box traffic is always observable, never silent.
//
// This package is dependency-free aside from core/config and speaks only about
// provider base URLs, so it can be reused by the server and by tooling.
package sovereign

import (
	"net"
	"net/url"
	"strings"

	"github.com/llmux/llmux/core/config"
)

// Locality classifies where a provider's traffic goes.
type Locality string

const (
	// Local means the backend runs on THIS box: a loopback address
	// (localhost, 127.0.0.0/8, ::1) or a unix socket. Data never leaves.
	Local Locality = "local"
	// Egress means calling the provider sends the request off the box to a
	// remote endpoint. Permitted only with explicit operator opt-in.
	Egress Locality = "egress"
)

// Tier is the sovereignty tier — "where your AI runs" — for a provider. The
// four values are ordered most→least private. The string values are part of the
// cross-repo contract (llmux + the vulos assistant use the SAME strings).
type Tier string

const (
	// TierLocal: inference on THIS box (loopback / unix socket). Always allowed.
	TierLocal Tier = "local"
	// TierSovereign: a Vulos-operated trusted endpoint (in-region, no-train,
	// isolated), operator-declared. Allowed by default.
	TierSovereign Tier = "sovereign"
	// TierBrokered: a named third party under a no-train agreement,
	// operator-configured. Allowed only when opted in.
	TierBrokered Tier = "brokered"
	// TierExternal: any other off-box endpoint (may mine/train). Blocked unless
	// allow_egress. This is the fail-closed default for anything unclassifiable.
	TierExternal Tier = "external"
)

// Label returns the honest human-facing UI label for a tier (cross-repo
// contract — must match the vulos assistant badge/picker verbatim).
func (t Tier) Label() string {
	switch t {
	case TierLocal:
		return "On your device"
	case TierSovereign:
		return "Vulos sovereign · in-region, no-train"
	case TierBrokered:
		return "Brokered · no-train"
	default:
		return "External · not private"
	}
}

// TierOf classifies a provider into a sovereignty tier from its base URL and its
// operator-declared tier marking (config `tier`, "" = auto). Rules:
//
//   - A loopback / unix-socket target is ALWAYS local, regardless of marking —
//     you cannot "downgrade" an on-box endpoint, and you cannot mislabel it.
//   - An off-box endpoint honors an explicit sovereign/brokered/external marking
//     (these are operator trust declarations).
//   - An unmarked ("") off-box endpoint derives from locality: external. Nothing
//     silently upgrades to sovereign/brokered.
//   - It fails CLOSED: an off-box endpoint marked "local", or marked with any
//     unrecognized value, is treated as external.
func TierOf(baseURL, marked string) Tier {
	if LocalityOf(baseURL) == Local {
		return TierLocal
	}
	// Off-box: honor an explicit trust declaration; otherwise fail closed.
	switch Tier(strings.ToLower(strings.TrimSpace(marked))) {
	case TierSovereign:
		return TierSovereign
	case TierBrokered:
		return TierBrokered
	default:
		// Unmarked, marked "external", marked (dishonestly) "local", or any
		// unknown value → external. Nothing off-box silently upgrades.
		return TierExternal
	}
}

// LocalityOf classifies a provider base URL. Only a loopback host (localhost,
// 127.0.0.0/8, ::1) or a unix-socket / on-box filesystem-path target is Local;
// everything else — public hosts, private LAN addresses, docker service names —
// is Egress. It fails CLOSED: an EMPTY base URL (e.g. an adapter like Bedrock
// that reaches a cloud region without a base URL) and any unparseable/hostless
// URL are treated as Egress, so a provider is never assumed on-box by accident.
func LocalityOf(baseURL string) Locality {
	s := strings.TrimSpace(baseURL)
	if s == "" {
		// No on-box endpoint we can point to. Fail closed: cloud adapters with
		// no base URL (Bedrock) must not be mistaken for a local server.
		return Egress
	}
	// unix:///path or a bare filesystem path => on-box socket.
	if strings.HasPrefix(s, "unix:") || strings.HasPrefix(s, "/") {
		return Local
	}
	host := hostOf(s)
	if host == "" {
		return Egress // unparseable / hostless: fail closed
	}
	if isLoopbackHost(host) {
		return Local
	}
	return Egress
}

// hostOf extracts the hostname from a base URL, or "" if none can be parsed.
func hostOf(baseURL string) string {
	s := baseURL
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// isLoopbackHost reports whether host names this machine's loopback interface.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Decision is the sovereignty verdict for one provider.
type Decision struct {
	Provider string
	BaseURL  string
	// Locality is the coarse on-box/off-box classification, retained for the
	// existing egress accounting and for callers that only care whether data
	// leaves the box.
	Locality Locality
	// Tier is the fine-grained sovereignty tier — "where your AI runs".
	Tier Tier
	// Allowed reports whether a request may be dispatched to this provider:
	//   local, sovereign          → always allowed
	//   brokered                  → allowed only with allow_brokered (or allow_egress)
	//   external                  → allowed only with allow_egress
	Allowed bool
}

// Label returns the honest human-facing tier label for this decision.
func (d Decision) Label() string { return d.Tier.Label() }

// allowedForTier applies the tiered default-deny policy for one provider config.
func allowedForTier(t Tier, allowBrokered, allowEgress bool) bool {
	switch t {
	case TierLocal, TierSovereign:
		return true
	case TierBrokered:
		return allowBrokered || allowEgress
	default: // TierExternal and anything unclassifiable → fail closed
		return allowEgress
	}
}

// Policy classifies the configured providers and enforces the local-default
// guarantee. It is built once from config and consulted before every dispatch.
type Policy struct {
	byName map[string]Decision
}

// NewPolicy builds a Policy from provider configs. Each provider is classified
// into a tier (from base URL + declared tier) and allowed per the tiered
// default-deny policy: local/sovereign always; brokered on opt-in; external only
// with allow_egress.
func NewPolicy(cfgs []config.ProviderConfig) *Policy {
	m := make(map[string]Decision, len(cfgs))
	for _, c := range cfgs {
		loc := LocalityOf(c.BaseURL)
		tier := TierOf(c.BaseURL, c.Tier)
		m[c.Name] = Decision{
			Provider: c.Name,
			BaseURL:  c.BaseURL,
			Locality: loc,
			Tier:     tier,
			Allowed:  allowedForTier(tier, c.AllowBrokered, c.AllowEgress),
		}
	}
	return &Policy{byName: m}
}

// Check returns the sovereignty decision for a provider name. An unknown
// provider fails CLOSED: it is reported as a denied external target.
func (p *Policy) Check(provider string) Decision {
	if d, ok := p.byName[provider]; ok {
		return d
	}
	return Decision{Provider: provider, Locality: Egress, Tier: TierExternal, Allowed: false}
}

// AllowedEgress returns the names of providers explicitly opted in to leave the
// box, so startup/health can label exactly what may egress.
func (p *Policy) AllowedEgress() []string {
	var out []string
	for name, d := range p.byName {
		if d.Locality == Egress && d.Allowed {
			out = append(out, name)
		}
	}
	return out
}

// TierSummary groups the ALLOWED providers by tier (for /health disclosure),
// reporting exactly which providers run in each sovereignty tier. Blocked
// providers are omitted here (AllowedEgress + the per-provider tier already
// disclose them); this reports what is permitted, per tier.
func (p *Policy) TierSummary() map[string][]string {
	out := map[string][]string{}
	for _, d := range p.byName {
		if !d.Allowed {
			continue
		}
		out[string(d.Tier)] = append(out[string(d.Tier)], d.Provider)
	}
	return out
}

// Decisions returns a snapshot of every provider's decision (for disclosure).
func (p *Policy) Decisions() []Decision {
	out := make([]Decision, 0, len(p.byName))
	for _, d := range p.byName {
		out = append(out, d)
	}
	return out
}

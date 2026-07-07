package sovereign

import (
	"reflect"
	"sort"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// TestLocalityEdgeCasesNotAutoTrusted locks the fail-closed classifier against
// the classic "trusted-sounding host" mistakes: .internal / .local / .lan /
// private-DNS names, mDNS-style suffixes, and cloud metadata IPs are NOT
// loopback, so they must classify as Egress (off-box). Only a genuine loopback
// address, .localhost, or a unix/on-box path is Local. A single accidental
// "these look internal, trust them" rule here would silently exfiltrate data.
func TestLocalityEdgeCasesNotAutoTrusted(t *testing.T) {
	cases := []struct {
		url  string
		want Locality
	}{
		// .internal / .local / .lan are NOT loopback — corporate/mDNS names still
		// leave THIS box, so they must be Egress (fail closed).
		{"http://ollama.internal:11434/v1", Egress},
		{"http://box.local:11434/v1", Egress},
		{"http://server.lan/v1", Egress},
		{"http://my-service.svc.cluster.local/v1", Egress},
		{"http://ollama.localdomain/v1", Egress},
		// A hostname merely CONTAINING "localhost" as a substring (not the label
		// nor a .localhost suffix) must not be trusted.
		{"http://localhost.evil.com/v1", Egress},
		{"http://notlocalhost/v1", Egress},
		// Cloud metadata / link-local / private ranges are off-box.
		{"http://169.254.169.254/latest", Egress},
		{"http://172.16.0.9:11434/v1", Egress},
		// IPv6 unique-local / link-local are not loopback.
		{"http://[fd00::1]:11434/v1", Egress},
		{"http://[fe80::1]:11434/v1", Egress},
		// Genuine loopback forms remain Local.
		{"http://127.255.255.254/v1", Local}, // all of 127.0.0.0/8 is loopback
		{"http://[::1]/v1", Local},
		{"http://sub.localhost/v1", Local},
		// Malformed / hostless URLs fail closed to Egress.
		{"http://", Egress},
		{"://nonsense", Egress},
		{"ht!tp://%zz", Egress},
	}
	for _, c := range cases {
		if got := LocalityOf(c.url); got != c.want {
			t.Errorf("LocalityOf(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestTierOfInternalLocalNotAutoTrusted is the tier-level companion: an
// off-box .internal/.local endpoint, even if the operator (dishonestly or
// mistakenly) marked it "local", must NOT be treated as local. Unmarked, it is
// external; a bogus marking also fails closed to external.
func TestTierOfInternalLocalNotAutoTrusted(t *testing.T) {
	cases := []struct {
		url, marked string
		want        Tier
	}{
		{"http://ollama.internal:11434/v1", "", TierExternal},
		{"http://box.local:11434/v1", "local", TierExternal},        // cannot self-declare local off-box
		{"http://svc.cluster.local/v1", "sovereign", TierSovereign}, // explicit trust IS honored
		{"http://169.254.169.254/latest", "", TierExternal},
	}
	for _, c := range cases {
		if got := TierOf(c.url, c.marked); got != c.want {
			t.Errorf("TierOf(%q, %q) = %q, want %q", c.url, c.marked, got, c.want)
		}
	}
}

// TestDecisionLabel proves the per-Decision honest label passes the tier's
// human-facing string through unchanged (this is the string the vulos assistant
// badge renders and the /health surface discloses).
func TestDecisionLabel(t *testing.T) {
	cases := map[Tier]string{
		TierLocal:     "On your device",
		TierSovereign: "Operator-declared endpoint (unverified)",
		TierBrokered:  "Brokered · no-train (operator-configured)",
		TierExternal:  "External · not private",
	}
	for tier, want := range cases {
		d := Decision{Tier: tier}
		if got := d.Label(); got != want {
			t.Errorf("Decision{Tier:%q}.Label() = %q, want %q", tier, got, want)
		}
	}
}

// TestTierSummaryGroupsAllowedOnly proves the /health disclosure helper groups
// ALLOWED providers by tier and OMITS blocked ones (blocked providers are
// disclosed separately via AllowedEgress/Decisions). A regression that leaked a
// blocked provider into the "permitted" summary would misrepresent the posture.
func TestTierSummaryGroupsAllowedOnly(t *testing.T) {
	p := NewPolicy([]config.ProviderConfig{
		{Name: "ollama", BaseURL: "http://localhost:11434/v1"},                                            // local  → allowed
		{Name: "pool", BaseURL: "https://pool.eu.vulos.net/v1", Tier: "sovereign"},                        // sovereign → allowed
		{Name: "brk-on", BaseURL: "https://broker.example.com/v1", Tier: "brokered", AllowBrokered: true}, // allowed
		{Name: "brk-off", BaseURL: "https://broker.example.com/v1", Tier: "brokered"},                     // BLOCKED → omitted
		{Name: "ext-on", BaseURL: "https://api.openai.com/v1", AllowEgress: true},                         // external → allowed
		{Name: "ext-off", BaseURL: "https://api.openai.com/v1"},                                           // BLOCKED → omitted
	})

	got := p.TierSummary()
	for k := range got {
		sort.Strings(got[k])
	}
	want := map[string][]string{
		"local":     {"ollama"},
		"sovereign": {"pool"},
		"brokered":  {"brk-on"},
		"external":  {"ext-on"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TierSummary() = %v, want %v", got, want)
	}
	// Blocked providers must NOT appear anywhere in the permitted summary.
	for _, list := range got {
		for _, name := range list {
			if name == "brk-off" || name == "ext-off" {
				t.Fatalf("blocked provider %q leaked into TierSummary %v", name, got)
			}
		}
	}
}

// TestDecisionsSnapshotIncludesBlocked proves Decisions() returns EVERY
// provider (allowed and blocked) with its classification, so the disclosure /
// startup log can report exactly what is blocked. Unlike TierSummary, nothing
// is filtered here.
func TestDecisionsSnapshotIncludesBlocked(t *testing.T) {
	p := NewPolicy([]config.ProviderConfig{
		{Name: "ollama", BaseURL: "http://localhost:11434/v1"},
		{Name: "ext-off", BaseURL: "https://api.openai.com/v1"}, // blocked
	})
	ds := p.Decisions()
	if len(ds) != 2 {
		t.Fatalf("Decisions() len = %d, want 2", len(ds))
	}
	byName := map[string]Decision{}
	for _, d := range ds {
		byName[d.Provider] = d
	}
	if d := byName["ollama"]; d.Tier != TierLocal || !d.Allowed {
		t.Errorf("ollama decision = %+v, want local+allowed", d)
	}
	if d := byName["ext-off"]; d.Tier != TierExternal || d.Allowed {
		t.Errorf("ext-off decision = %+v, want external+blocked (present in snapshot)", d)
	}
}

// TestAllowedEgressExcludesBlockedAndLocal proves the AllowedEgress disclosure
// lists ONLY providers that both leave the box AND are permitted — never a
// blocked egress, never a local provider.
func TestAllowedEgressExcludesBlockedAndLocal(t *testing.T) {
	p := NewPolicy([]config.ProviderConfig{
		{Name: "ollama", BaseURL: "http://localhost:11434/v1"},                    // local (not egress)
		{Name: "sov", BaseURL: "https://pool.eu.vulos.net/v1", Tier: "sovereign"}, // egress, allowed
		{Name: "ext-off", BaseURL: "https://api.openai.com/v1"},                   // egress, BLOCKED
		{Name: "ext-on", BaseURL: "https://api.openai.com/v1", AllowEgress: true}, // egress, allowed
	})
	got := p.AllowedEgress()
	sort.Strings(got)
	want := []string{"ext-on", "sov"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedEgress() = %v, want %v", got, want)
	}
}

package sovereign

import (
	"testing"

	"github.com/llmux/llmux/core/config"
)

func TestLocalityOf(t *testing.T) {
	cases := []struct {
		url  string
		want Locality
	}{
		// On-box: loopback + unix socket + empty.
		{"http://localhost:11434/v1", Local},
		{"http://127.0.0.1:8080/v1", Local},
		{"https://127.0.0.1/v1", Local},
		{"http://[::1]:11434/v1", Local},
		{"http://LOCALHOST:11434/v1", Local},
		{"http://foo.localhost:11434/v1", Local},
		{"unix:///run/llmux.sock", Local},
		{"/run/ollama.sock", Local},
		// Empty base URL fails closed to Egress (e.g. Bedrock reaches AWS with
		// no base URL and must never be mistaken for an on-box server).
		{"", Egress},
		// Off-box: public + private LAN + docker service names all egress.
		{"https://api.openai.com/v1", Egress},
		{"https://api.anthropic.com/v1", Egress},
		{"http://192.168.1.50:11434/v1", Egress},
		{"http://10.0.0.5:11434/v1", Egress},
		{"http://ollama:11434/v1", Egress},
		{"http://example.com", Egress},
	}
	for _, c := range cases {
		if got := LocalityOf(c.url); got != c.want {
			t.Errorf("LocalityOf(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestTierOf(t *testing.T) {
	cases := []struct {
		url    string
		marked string
		want   Tier
	}{
		// Loopback / on-box is ALWAYS local, regardless of any marking.
		{"http://localhost:11434/v1", "", TierLocal},
		{"http://127.0.0.1:8080/v1", "external", TierLocal}, // cannot mislabel on-box
		{"unix:///run/llmux.sock", "brokered", TierLocal},
		// Off-box endpoints honor an explicit sovereign/brokered declaration.
		{"https://pool.eu.vulos.net/v1", "sovereign", TierSovereign},
		{"https://broker.example.com/v1", "brokered", TierBrokered},
		{"https://api.openai.com/v1", "external", TierExternal},
		// Unmarked off-box derives from locality → external (nothing upgrades).
		{"https://api.openai.com/v1", "", TierExternal},
		{"http://192.168.1.50:11434/v1", "", TierExternal},
		// Fail closed: an off-box endpoint dishonestly marked "local", or with an
		// unknown value, is treated as external.
		{"https://api.openai.com/v1", "local", TierExternal},
		{"https://api.openai.com/v1", "bogus", TierExternal},
		// Empty base URL (cloud adapter) is off-box → external unless marked.
		{"", "", TierExternal},
	}
	for _, c := range cases {
		if got := TierOf(c.url, c.marked); got != c.want {
			t.Errorf("TierOf(%q, %q) = %q, want %q", c.url, c.marked, got, c.want)
		}
	}
}

// TestPolicyTiers exercises the tiered default-deny policy end to end: local and
// sovereign are allowed with NO opt-in; brokered is blocked until opted in; and
// external is blocked unless allow_egress.
func TestPolicyTiers(t *testing.T) {
	p := NewPolicy([]config.ProviderConfig{
		{Name: "local", BaseURL: "http://localhost:11434/v1"},
		{Name: "sov", BaseURL: "https://pool.eu.vulos.net/v1", Tier: "sovereign"},
		{Name: "brk-off", BaseURL: "https://broker.example.com/v1", Tier: "brokered"},
		{Name: "brk-on", BaseURL: "https://broker.example.com/v1", Tier: "brokered", AllowBrokered: true},
		{Name: "brk-egress", BaseURL: "https://broker.example.com/v1", Tier: "brokered", AllowEgress: true},
		{Name: "ext-off", BaseURL: "https://api.openai.com/v1", Tier: "external"},
		{Name: "ext-on", BaseURL: "https://api.openai.com/v1", Tier: "external", AllowEgress: true},
		{Name: "ext-brokeredflag", BaseURL: "https://api.openai.com/v1", AllowBrokered: true}, // brokered flag does NOT unlock external
	})

	want := []struct {
		name    string
		tier    Tier
		allowed bool
	}{
		{"local", TierLocal, true},
		{"sov", TierSovereign, true},
		{"brk-off", TierBrokered, false},
		{"brk-on", TierBrokered, true},
		{"brk-egress", TierBrokered, true},
		{"ext-off", TierExternal, false},
		{"ext-on", TierExternal, true},
		{"ext-brokeredflag", TierExternal, false},
	}
	for _, w := range want {
		d := p.Check(w.name)
		if d.Tier != w.tier || d.Allowed != w.allowed {
			t.Errorf("Check(%q) = tier=%q allowed=%v, want tier=%q allowed=%v", w.name, d.Tier, d.Allowed, w.tier, w.allowed)
		}
	}

	// Unknown provider fails closed to external/blocked.
	if d := p.Check("ghost"); d.Tier != TierExternal || d.Allowed {
		t.Errorf("unknown provider must fail closed to external/blocked; got %+v", d)
	}
}

func TestTierLabels(t *testing.T) {
	cases := map[Tier]string{
		TierLocal:     "On your device",
		TierSovereign: "Vulos sovereign · in-region, no-train",
		TierBrokered:  "Brokered · no-train",
		TierExternal:  "External · not private",
	}
	for tier, label := range cases {
		if got := tier.Label(); got != label {
			t.Errorf("Tier(%q).Label() = %q, want %q", tier, got, label)
		}
	}
}

func TestPolicyDefaultDeniesEgress(t *testing.T) {
	p := NewPolicy([]config.ProviderConfig{
		{Name: "local", BaseURL: "http://localhost:11434/v1"},
		{Name: "openai", BaseURL: "https://api.openai.com/v1"},                        // not opted in
		{Name: "broker", BaseURL: "https://broker.example.com/v1", AllowEgress: true}, // opted in
	})

	if d := p.Check("local"); !d.Allowed || d.Locality != Local {
		t.Errorf("local: got %+v, want allowed local", d)
	}
	if d := p.Check("openai"); d.Allowed || d.Locality != Egress {
		t.Errorf("openai (no opt-in) must be a DENIED egress; got %+v", d)
	}
	if d := p.Check("broker"); !d.Allowed || d.Locality != Egress {
		t.Errorf("broker (opted in) must be an ALLOWED egress; got %+v", d)
	}
	// Unknown providers fail closed.
	if d := p.Check("ghost"); d.Allowed {
		t.Errorf("unknown provider must fail closed; got %+v", d)
	}

	eg := p.AllowedEgress()
	if len(eg) != 1 || eg[0] != "broker" {
		t.Errorf("AllowedEgress = %v, want [broker]", eg)
	}
}

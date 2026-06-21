package providers

import (
	"testing"

	"github.com/llmux/llmux/core/config"
)

func TestBuild_EachType(t *testing.T) {
	cfgs := []config.ProviderConfig{
		{Name: "pass", Type: config.TypePassthrough, BaseURL: "https://x/v1"},
		{Name: "anth", Type: config.TypeAnthropic, BaseURL: "https://a/v1"},
		{Name: "gem", Type: config.TypeGemini, BaseURL: "https://g/v1"},
		{Name: "coh", Type: config.TypeCohere, BaseURL: "https://c/v2"},
		{Name: "bed", Type: config.TypeBedrock},
	}

	reg, err := Build(cfgs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, name := range []string{"pass", "anth", "gem", "coh", "bed"} {
		p, ok := reg.Get(name)
		if !ok {
			t.Errorf("provider %q not registered", name)
			continue
		}
		if p.Name() != name {
			t.Errorf("provider registered under %q has Name() = %q", name, p.Name())
		}
	}

	if got := len(reg.Names()); got != 5 {
		t.Errorf("registry has %d providers, want 5", got)
	}
}

func TestBuild_UnknownTypeSkipped(t *testing.T) {
	cfgs := []config.ProviderConfig{
		{Name: "good", Type: config.TypePassthrough},
		{Name: "weird", Type: config.ProviderType("does-not-exist")},
		{Name: "empty", Type: config.ProviderType("")},
	}

	reg, err := Build(cfgs)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := reg.Get("good"); !ok {
		t.Error("expected good provider registered")
	}
	if _, ok := reg.Get("weird"); ok {
		t.Error("unknown-type provider should be skipped")
	}
	if _, ok := reg.Get("empty"); ok {
		t.Error("empty-type provider should be skipped")
	}
	if got := len(reg.Names()); got != 1 {
		t.Errorf("registry has %d providers, want 1", got)
	}
}

func TestBuild_DuplicateNames(t *testing.T) {
	cfgs := []config.ProviderConfig{
		{Name: "dup", Type: config.TypePassthrough},
		{Name: "dup", Type: config.TypeAnthropic},
	}
	if _, err := Build(cfgs); err == nil {
		t.Fatal("expected error for duplicate provider names")
	}
}

func TestBuild_Empty(t *testing.T) {
	reg, err := Build(nil)
	if err != nil {
		t.Fatalf("Build(nil): %v", err)
	}
	if got := len(reg.Names()); got != 0 {
		t.Errorf("empty build should have 0 providers, got %d", got)
	}
}

func TestStability(t *testing.T) {
	cases := []struct {
		typ  config.ProviderType
		want string
	}{
		{config.TypePassthrough, "stable"},
		{config.TypeAnthropic, "beta"},
		{config.TypeGemini, "beta"},
		{config.TypeCohere, "experimental"},
		{config.TypeBedrock, "experimental"},
		{config.ProviderType("mystery"), "unknown"},
		{config.ProviderType(""), "unknown"},
	}
	for _, tc := range cases {
		if got := Stability(tc.typ); got != tc.want {
			t.Errorf("Stability(%q) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

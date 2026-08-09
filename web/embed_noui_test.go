//go:build noui

package webui

import "testing"

// Under the `noui` tag the console is genuinely absent — not empty-but-present.
// Assert both halves so a future edit cannot leave Enabled() true while the
// embed returns nothing (core/server would then serve a blank /ui).
func TestConsoleAbsentUnderNoUITag(t *testing.T) {
	if Enabled() {
		t.Fatal("Enabled() is true under -tags noui — the stub reports a console that was not built")
	}
	if len(HTML()) != 0 {
		t.Fatalf("HTML() returned %d bytes under -tags noui, want none", len(HTML()))
	}
	if len(Licenses()) != 0 {
		t.Fatalf("Licenses() returned %d bytes under -tags noui, want none", len(Licenses()))
	}
}

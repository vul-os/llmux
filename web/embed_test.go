package webui

import (
	"strings"
	"testing"
)

// The admin console is now ONE hand-written file with no build step: ui.html
// IS the source, so there is nothing that can go stale against it the way a
// committed dist/ could against web/src. What can still break silently:
//
//   - go:embed's pattern stops matching (renamed/deleted file) and ships an
//     empty or truncated page.
//   - an edit removes something the dashboard actually needs (a tab, the
//     localStorage keys the settings bar depends on, an endpoint it calls).
//   - the page grows a reference to an external origin — a CDN script, a
//     hosted font, a remote stylesheet — which defeats the entire point of
//     an offline-embedded, no-build UI: it must render with no network
//     access beyond the gateway itself.
//
// Each test asserts a coverage floor before it inspects anything, so a check
// that silently stopped checking (an empty embed, a regex that no longer
// matches) fails loudly instead of reporting a vacuous pass.

// minHTMLBytes is the coverage floor for TestEmbeddedHTMLIsPresent. ui.html
// carries real inline CSS and JS for three views; a page collapsed to a
// placeholder or an empty go:embed would be nowhere near this size.
const minHTMLBytes = 8000

func TestEmbeddedHTMLIsPresent(t *testing.T) {
	html := HTML()
	if len(html) < minHTMLBytes {
		t.Fatalf("embedded ui.html is %d bytes, want at least %d — the embed is empty or truncated, "+
			"so /ui would serve nothing usable", len(html), minHTMLBytes)
	}
}

// TestEmbeddedHTMLHasRequiredElements checks the page still contains the
// pieces the dashboard's behaviour depends on: all three tabs, the
// localStorage keys the settings bar persists to (an operator's saved
// gateway URL / master key must keep working across a UI rewrite), and the
// three admin/catalog endpoints it calls.
func TestEmbeddedHTMLHasRequiredElements(t *testing.T) {
	html := string(HTML())
	required := []string{
		"<style", "<script", // no external stylesheet/script — everything inline
		"data-tab=\"usage\"", "data-tab=\"keys\"", "data-tab=\"models\"",
		"llmux_api_base", "llmux_master_key", // same localStorage slots as before
		"/admin/usage", "/admin/keys", "/v1/models", // the three JSON endpoints the UI reads
		"llmux", // the wordmark
	}
	for _, want := range required {
		if !strings.Contains(html, want) {
			t.Errorf("embedded ui.html does not contain %q — the dashboard is missing something it needs", want)
		}
	}
}

// TestEmbeddedHTMLReferencesNoExternalOrigin is the property that actually
// matters for an offline-embedded UI: it must load NO external resource.
// Plain navigational links (the "docs" link to vulos.org) are fine — a user
// clicking off-page is not the page depending on that origin to render — so
// this specifically forbids resource-loading constructs: a src="http(s)://"
// attribute, a CSS @import or url(http...), an external stylesheet link, or
// any reference to a CDN host.
func TestEmbeddedHTMLReferencesNoExternalOrigin(t *testing.T) {
	html := strings.ToLower(string(HTML()))
	forbidden := []string{
		`src="http://`, `src="https://`, `src='http://`, `src='https://`,
		`@import`, `url(http://`, `url(https://`,
		`rel="stylesheet" href="http`, `rel="preload"`, `rel="preconnect"`, `rel="dns-prefetch"`,
		"cdn.", "googleapis.com", "gstatic.com", "jsdelivr", "unpkg.com", "cdnjs",
	}
	for _, pat := range forbidden {
		if strings.Contains(html, pat) {
			t.Errorf("embedded ui.html contains %q — an offline-embedded admin console must load no external resource", pat)
		}
	}
}

// TestLicensesTxtIsPresent guards the Go-dependency attribution served at
// /ui/licenses.txt: it must actually be the generated notices file, not an
// empty or missing embed.
func TestLicensesTxtIsPresent(t *testing.T) {
	txt := string(Licenses())
	if len(txt) < 500 {
		t.Fatalf("embedded THIRD-PARTY-NOTICES-GO.txt is %d bytes, want at least 500 — "+
			"Go-dependency attribution would be missing at /ui/licenses.txt", len(txt))
	}
	if !strings.Contains(txt, "THIRD-PARTY NOTICES") {
		t.Errorf("embedded licenses text does not look like the notices file (missing header)")
	}
}

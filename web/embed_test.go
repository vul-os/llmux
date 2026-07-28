package webui

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The bundle in web/dist is COMMITTED and compiled into the binary by the
// go:embed directive in embed.go, so what a user's browser loads is whatever is
// in git — not whatever CI last built. Byte-for-byte freshness against web/src
// needs Node and is enforced by scripts/check-web-dist.sh in CI.
//
// These tests are the half that needs no toolchain and therefore runs in every
// `go test ./...`: they check the embedded bundle is INTERNALLY COHERENT. The
// specific failure they exist to catch is a partially updated dist — a new
// index.html committed alongside the old content-hashed chunks, or the reverse.
// Vite renames every chunk on content change, so a half-committed rebuild
// leaves index.html pointing at files that are not there, and the gateway
// serves a 200 with a white screen. `go build` cannot see that; go:embed
// happily embeds an index.html full of dangling links.
//
// Neither test can pass by doing nothing: each asserts a coverage floor on how
// much it actually inspected before inspecting it.

// minEmbeddedFiles is the coverage floor. The committed bundle is 13 files
// (index.html, licenses.txt, 4 hashed assets, 6 icons/images, 1 screenshot).
// If the embedded tree ever collapses — an emptied dist/, a botched go:embed
// pattern, a wrong fs.Sub root — the walks below would find nothing to check
// and would otherwise report success.
const minEmbeddedFiles = 10

// assetRef matches the bundled asset paths Vite writes into index.html. The
// build uses base "/ui/", so references appear as "/ui/assets/index-HASH.js".
var assetRef = regexp.MustCompile(`assets/[A-Za-z0-9._-]+`)

// embeddedFiles returns every file in the embedded bundle, rooted at dist/.
func embeddedFiles(t *testing.T) []string {
	t.Helper()
	sub, err := FS()
	if err != nil {
		t.Fatalf("webui: FS() failed: %v — the embedded UI is unreachable and NOTHING was verified", err)
	}
	var out []string
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("webui: walking the embedded bundle failed: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestEmbeddedBundleIsPresent asserts the binary actually carries a UI. A
// gateway that embeds an empty dist/ builds, starts, tests green, and serves
// nothing at /ui.
func TestEmbeddedBundleIsPresent(t *testing.T) {
	files := embeddedFiles(t)
	if len(files) < minEmbeddedFiles {
		t.Fatalf("embedded web bundle has %d files, want at least %d — web/dist is empty or "+
			"truncated, so the binary would serve a broken /ui.\nEmbedded: %v\n"+
			"Rebuild with `make web` (npm --prefix web run build).",
			len(files), minEmbeddedFiles, files)
	}

	for _, required := range []string{"index.html", "licenses.txt"} {
		found := false
		for _, f := range files {
			if f == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("embedded bundle is missing %q — run `make web`", required)
		}
	}
}

// TestEmbeddedIndexHasNoDanglingAssets is the half-committed-rebuild guard:
// every content-hashed asset index.html asks the browser for must be embedded
// alongside it. This is what breaks when web/src changes and only some of
// web/dist is committed.
func TestEmbeddedIndexHasNoDanglingAssets(t *testing.T) {
	sub, err := FS()
	if err != nil {
		t.Fatalf("webui: FS() failed: %v", err)
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("webui: embedded index.html unreadable: %v — the bundle is broken", err)
	}

	present := map[string]bool{}
	for _, f := range embeddedFiles(t) {
		present[f] = true
	}

	seen := map[string]bool{}
	for _, ref := range assetRef.FindAllString(string(index), -1) {
		ref = strings.TrimPrefix(ref, "/")
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if !present[ref] {
			t.Errorf("embedded index.html references %q, which is NOT in the embedded bundle. "+
				"web/dist is half-updated: the gateway would serve /ui as a blank page. "+
				"Rebuild and commit the whole bundle (`make web`).", ref)
		}
	}

	// Coverage floor for THIS test specifically: a Vite build always emits at
	// least a JS entry and a CSS file into index.html. Finding fewer means the
	// regex stopped matching or index.html is a placeholder — either way the
	// loop above verified nothing and must not read as a pass.
	if len(seen) < 2 {
		t.Fatalf("index.html references %d bundled assets, want at least 2 (a JS entry and a "+
			"stylesheet). The dangling-reference check inspected almost nothing, so it proves "+
			"nothing. Referenced: %v", len(seen), seen)
	}

	var js, css bool
	for ref := range seen {
		switch {
		case strings.HasSuffix(ref, ".js"):
			js = true
		case strings.HasSuffix(ref, ".css"):
			css = true
		}
	}
	if !js || !css {
		t.Errorf("index.html should reference both a JS entry and a stylesheet (js=%v css=%v); "+
			"got %v — this does not look like a real Vite build", js, css, seen)
	}
}

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// root locates the module root for the tests, failing loudly rather than
// skipping — a docs guard that quietly does nothing is worse than none.
func root(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	r, err := findRoot(cwd)
	if err != nil {
		t.Fatalf("site/gen: %v — the docs guards verified NOTHING", err)
	}
	return r
}

// TestSiteDocsInSync is the anti-rot guard: site/docs must be exactly what the
// generator produces from docs/. Edit docs/api.md and forget to regenerate, and
// this fails naming the file.
func TestSiteDocsInSync(t *testing.T) {
	r := root(t)
	want, err := generate(r)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(want) != len(pages) || len(pages) < 10 {
		t.Fatalf("generator produced %d of %d pages — too few to be a real check", len(want), len(pages))
	}

	dir := filepath.Join(r, "site", "docs")
	for name, body := range want {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("site/docs/%s is missing: %v — run `go run ./site/gen`", name, err)
			continue
		}
		if string(got) != body {
			t.Errorf("site/docs/%s is STALE (does not match the canonical source). "+
				"docs/ is the single source of truth; run `go run ./site/gen` to regenerate.", name)
		}
	}

	// No orphans: a file in site/docs that the generator does not produce is a
	// hand-written copy that will rot.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read site/docs: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if _, ok := want[e.Name()]; !ok {
			t.Errorf("site/docs/%s is not generated from anything — either add it to `pages` in "+
				"site/gen/main.go or delete it; hand-maintained copies of docs/ go stale", e.Name())
		}
	}
}

// siteLinkRE matches markdown link targets in a generated site doc.
var siteLinkRE = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestSiteDocsHaveNoBrokenLinks checks the generated bundle the way a visitor
// experiences it: every relative link must resolve to a file that actually
// ships in site/docs. Links that cannot (repo-root files, web/docs) must have
// been rewritten to absolute URLs by the generator.
func TestSiteDocsHaveNoBrokenLinks(t *testing.T) {
	r := root(t)
	dir := filepath.Join(r, "site", "docs")

	files, err := generate(r)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	links := 0
	for name, body := range files {
		for _, m := range siteLinkRE.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			links++
			file := target
			if i := strings.IndexByte(target, '#'); i != -1 {
				file = target[:i]
			}
			if file == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				t.Errorf("BROKEN LINK: site/docs/%s links to %q, which does not ship in the site bundle",
					name, target)
			}
		}
	}
	if links == 0 {
		t.Fatal("no relative links were checked — the link scan is broken and verified nothing")
	}
	t.Logf("site docs: %d relative links checked across %d pages", links, len(files))
}

// TestSiteDocsNavMatchesGenerator keeps site/docs.html's sidebar and the
// generator's page list from drifting apart: a nav entry for a page nobody
// generates is a 404 in the viewer, and a generated page missing from the nav
// is unreachable.
func TestSiteDocsNavMatchesGenerator(t *testing.T) {
	r := root(t)
	html, err := os.ReadFile(filepath.Join(r, "site", "docs.html"))
	if err != nil {
		t.Fatalf("read site/docs.html: %v", err)
	}
	navRE := regexp.MustCompile(`"path"\s*:\s*"\./docs/([^"]+)"`)
	var nav []string
	for _, m := range navRE.FindAllStringSubmatch(string(html), -1) {
		nav = append(nav, m[1])
	}
	if len(nav) == 0 {
		t.Fatal("found no doc entries in site/docs.html — the nav scan is broken and verified nothing")
	}

	var generated []string
	for _, p := range pages {
		generated = append(generated, p.dst)
	}
	sort.Strings(nav)
	sort.Strings(generated)
	if strings.Join(nav, ",") != strings.Join(generated, ",") {
		t.Errorf("site/docs.html nav and the generator disagree:\n  nav:       %v\n  generated: %v",
			nav, generated)
	}
}

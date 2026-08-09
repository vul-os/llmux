package server

import (
	"net/http"

	webui "github.com/vul-os/llmux/web"
)

// The admin console is optional in two independent ways:
//
//   - Options.UI (default TRUE, so `llmux serve` is unchanged) decides whether
//     this build MOUNTS it. An API-only gateway sets it false and /ui 404s.
//   - The `noui` build tag decides whether it is COMPILED IN at all. With the
//     tag, webui.Enabled() is false and /ui serves a small JSON stub saying so,
//     rather than pretending the route does not exist.
//
// Only this package may import web/ — core/gateway must stay reachable without
// it, so an embedding host never links the console it will not serve.

// mountUI serves the embedded admin console at /ui — a single hand-written
// HTML file with inline CSS/JS, no separate assets — plus its Go-dependency
// attribution at /ui/licenses.txt (a static sub-resource of the page, not a
// documented API route — see docs/api.md, which only lists "GET /ui").
// Both are public; the dashboard authenticates to /admin client-side with
// the master key (see authMW).
func (s *Server) mountUI() {
	if !s.opts.UI {
		return // API-only build: /ui is simply not a route.
	}

	s.mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})

	// A single subtree handler, matching how the embedded UI has always been
	// routed: everything under /ui/ is resolved here rather than as separate
	// registered patterns, so the app's internal pages/assets are not a
	// growing list of individually-documented API routes. Anything other
	// than the two paths below 404s — there is no third page or asset to
	// fall back to; the whole app is one document.
	s.mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		if !webui.Enabled() {
			writeUIStub(w)
			return
		}
		switch r.URL.Path {
		case "/ui/":
			writeUIHTML(w)
		case "/ui/licenses.txt":
			writeLicenses(w)
		default:
			http.NotFound(w, r)
		}
	})
}

// writeUIStub answers /ui/* on a `noui` build: the console is genuinely absent,
// so say so in machine-readable form instead of 404ing (which would read as
// "wrong URL") or serving a blank page.
func writeUIStub(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error": map[string]any{
			"message": "the admin console is not compiled into this binary (built with -tags noui); " +
				"the JSON API at /v1/*, /admin/* and /metrics is unaffected",
			"type": "invalid_request_error",
			"code": "ui_not_built",
		},
	})
}

// writeUIHTML writes the embedded admin console with a small set of security
// headers appropriate for a page that holds the admin master key: inline
// style/script are required (no build step means no hashed/nonced external
// bundle to point a strict CSP at instead), but no other origin may supply
// script, style, plugins, or a frame, and the page may not itself be framed.
// connect-src is left open because the gateway-URL field can point requests
// at any remote gateway the operator configures.
func writeUIHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"img-src 'self' data:; connect-src *; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Write(webui.HTML())
}

// writeLicenses writes the Go binary's third-party notices, embedded from
// web/THIRD-PARTY-NOTICES-GO.txt.
func writeLicenses(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(webui.Licenses())
}

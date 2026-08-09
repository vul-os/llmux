//go:build !noui

// Package webui embeds the admin console — a single hand-written HTML file
// with inline CSS and JS, no build step and no npm dependency — so the
// gateway can serve it at /ui with nothing beyond `go build`.
//
// The whole admin surface is three read-only JSON endpoints (GET /admin/keys,
// GET /admin/usage, GET /v1/models), so ui.html is the entire UI: no React,
// no bundler, no committed build artifact to go stale against its source.
// THIRD-PARTY-NOTICES-GO.txt documents the Go binary's own dependency graph
// (unrelated to the browser page, which ships zero third-party code) and is
// served alongside it at /ui/licenses.txt.
//
// Build with -tags noui to drop the embed entirely (see embed_noui.go): the
// binary shrinks by the size of ui.html and core/server mounts a small JSON
// stub at /ui instead of the console.
package webui

import _ "embed"

//go:embed ui.html
var uiHTML []byte

//go:embed THIRD-PARTY-NOTICES-GO.txt
var licensesTxt []byte

// HTML returns the embedded admin console page.
func HTML() []byte { return uiHTML }

// Licenses returns the embedded third-party notices for the Go binary's
// dependency graph, served at /ui/licenses.txt.
func Licenses() []byte { return licensesTxt }

// Enabled reports whether the admin console is compiled into this binary.
func Enabled() bool { return true }

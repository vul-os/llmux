// Package webui embeds the built Vite/React web app (landing, docs, and the
// admin/usage dashboard) so the gateway can serve it at /ui — no separate Node
// server at runtime. Run `npm --prefix web run build` to regenerate dist/.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// FS returns the built site rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(assets, "dist")
}

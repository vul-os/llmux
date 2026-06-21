package server

import (
	"io/fs"
	"net/http"
	"strings"

	webui "github.com/llmux/llmux/web"
)

// mountUI serves the embedded web app at /ui with SPA fallback (unknown paths
// return index.html so client-side routes work on refresh). Static assets are
// public; the dashboard authenticates to /admin client-side with the master key.
func (s *Server) mountUI() {
	sub, err := webui.FS()
	if err != nil {
		return
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return // no build present; skip mounting
	}
	fileServer := http.StripPrefix("/ui/", http.FileServer(http.FS(sub)))

	serve := func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/ui/")
		if rel == "" {
			writeIndex(w, index)
			return
		}
		if f, err := sub.Open(rel); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		writeIndex(w, index) // SPA fallback
	}

	s.mux.HandleFunc("GET /ui/", serve)
	s.mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
}

func writeIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(index)
}

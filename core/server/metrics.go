package server

import (
	"net/http"
	"strings"

	"github.com/vul-os/llmux/core/openai"
)

// knownPaths is the bounded set of route labels for metrics. Anything else is
// bucketed to "other" so a client spraying random paths can't blow up the
// metric cardinality / memory (unbounded-map DoS).
var knownPaths = map[string]bool{
	"/v1/chat/completions": true, "/v1/embeddings": true, "/v1/models": true,
	"/v1/catalog.json": true, "/v1/completions": true, "/v1/moderations": true,
	"/v1/images/generations": true, "/v1/audio/speech": true, "/v1/rerank": true,
	"/v1/audio/transcriptions": true, "/v1/audio/translations": true,
	"/v1/responses": true, "/health": true, "/metrics": true,
	"/admin/keys": true, "/admin/usage": true,
}

func pathBucket(p string) string {
	if knownPaths[p] {
		return p
	}
	if strings.HasPrefix(p, "/ui") {
		return "/ui/*"
	}
	return "other"
}

// statusRecorder captures the response status for metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush/Hijack passthrough so SSE streaming still works behind the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// metricsMW records request counts and in-flight gauge.
func (s *Server) metricsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := s.gw.Metrics()
		if r.URL.Path == "/metrics" {
			// Metrics leak operational/topology data; require the master key
			// when one is configured (open in local mode).
			if !s.isAdmin(r) {
				writeError(w, http.StatusUnauthorized,
					openai.NewError("metrics require the master key", "invalid_request_error", "invalid_api_key"))
				return
			}
			s.handleMetrics(w, r)
			return
		}
		m.AddInflight(1)
		defer m.AddInflight(-1)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		m.IncRequest(pathBucket(r.URL.Path), rec.status)
	})
}

// handleMetrics renders the gateway's counters in Prometheus text format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.gw.Metrics().WriteProm(w)
}

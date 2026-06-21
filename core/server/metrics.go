package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/llmux/llmux/core/openai"
)

// knownPaths is the bounded set of route labels for metrics. Anything else is
// bucketed to "other" so a client spraying random paths can't blow up the
// metric cardinality / memory (unbounded-map DoS).
var knownPaths = map[string]bool{
	"/v1/chat/completions": true, "/v1/embeddings": true, "/v1/models": true,
	"/v1/catalog.json": true, "/v1/completions": true, "/v1/moderations": true,
	"/v1/images/generations": true, "/v1/audio/speech": true, "/v1/rerank": true,
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

// Metrics holds lightweight in-process counters exported in Prometheus text
// format at /metrics. (Swap for a full client library in production.)
type Metrics struct {
	requestsTotal sync.Map // key "path|status" -> *int64
	inflight      int64
	upstreamErr   int64
	cacheHits     int64
}

// NewMetrics builds an empty metrics registry.
func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) incRequest(path string, status int) {
	key := fmt.Sprintf("%s|%d", path, status)
	v, _ := m.requestsTotal.LoadOrStore(key, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

func (m *Metrics) addInflight(d int64) { atomic.AddInt64(&m.inflight, d) }
func (m *Metrics) incUpstreamErr()     { atomic.AddInt64(&m.upstreamErr, 1) }
func (m *Metrics) incCacheHit()        { atomic.AddInt64(&m.cacheHits, 1) }

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
		s.metrics.addInflight(1)
		defer s.metrics.addInflight(-1)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.metrics.incRequest(pathBucket(r.URL.Path), rec.status)
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP llmux_requests_total Total HTTP requests by path and status.")
	fmt.Fprintln(w, "# TYPE llmux_requests_total counter")
	s.metrics.requestsTotal.Range(func(k, v any) bool {
		key := k.(string) // "path|status"
		path, status := key, 0
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				path, status = key[:i], atoiSafe(key[i+1:])
				break
			}
		}
		fmt.Fprintf(w, "llmux_requests_total{path=%q,status=\"%d\"} %d\n", path, status, atomic.LoadInt64(v.(*int64)))
		return true
	})
	fmt.Fprintln(w, "# HELP llmux_inflight_requests In-flight requests.")
	fmt.Fprintln(w, "# TYPE llmux_inflight_requests gauge")
	fmt.Fprintf(w, "llmux_inflight_requests %d\n", atomic.LoadInt64(&s.metrics.inflight))
	fmt.Fprintln(w, "# HELP llmux_upstream_errors_total Upstream provider errors.")
	fmt.Fprintln(w, "# TYPE llmux_upstream_errors_total counter")
	fmt.Fprintf(w, "llmux_upstream_errors_total %d\n", atomic.LoadInt64(&s.metrics.upstreamErr))
	fmt.Fprintln(w, "# HELP llmux_cache_hits_total Exact-cache hits.")
	fmt.Fprintln(w, "# TYPE llmux_cache_hits_total counter")
	fmt.Fprintf(w, "llmux_cache_hits_total %d\n", atomic.LoadInt64(&s.metrics.cacheHits))
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

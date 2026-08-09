package gateway

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Metrics holds lightweight in-process counters. The HTTP shell exports them in
// Prometheus text format at /metrics (see WriteProm); an embedding host can read
// the accessors directly or ignore them entirely.
type Metrics struct {
	requestsTotal sync.Map // key "path|status" -> *int64
	inflight      int64
	upstreamErr   int64
	cacheHits     int64
	egressBlocked int64
}

// NewMetrics builds an empty metrics registry.
func NewMetrics() *Metrics { return &Metrics{} }

func (m *Metrics) incUpstreamErr()   { atomic.AddInt64(&m.upstreamErr, 1) }
func (m *Metrics) incCacheHit()      { atomic.AddInt64(&m.cacheHits, 1) }
func (m *Metrics) incEgressBlocked() { atomic.AddInt64(&m.egressBlocked, 1) }

// IncRequest counts one served request by route label and status.
func (m *Metrics) IncRequest(path string, status int) {
	key := fmt.Sprintf("%s|%d", path, status)
	v, _ := m.requestsTotal.LoadOrStore(key, new(int64))
	atomic.AddInt64(v.(*int64), 1)
}

// AddInflight adjusts the in-flight request gauge.
func (m *Metrics) AddInflight(d int64) { atomic.AddInt64(&m.inflight, d) }

// IncUpstreamErr counts one upstream provider failure.
func (m *Metrics) IncUpstreamErr() { m.incUpstreamErr() }

// Inflight returns the current in-flight request count.
func (m *Metrics) Inflight() int64 { return atomic.LoadInt64(&m.inflight) }

// UpstreamErrors returns the cumulative upstream error count.
func (m *Metrics) UpstreamErrors() int64 { return atomic.LoadInt64(&m.upstreamErr) }

// CacheHits returns the cumulative exact/semantic cache hit count.
func (m *Metrics) CacheHits() int64 { return atomic.LoadInt64(&m.cacheHits) }

// EgressBlocked returns how many requests the sovereignty gate denied.
func (m *Metrics) EgressBlocked() int64 { return atomic.LoadInt64(&m.egressBlocked) }

// WriteProm renders the counters in Prometheus text format.
func (m *Metrics) WriteProm(w io.Writer) {
	fmt.Fprintln(w, "# HELP llmux_requests_total Total HTTP requests by path and status.")
	fmt.Fprintln(w, "# TYPE llmux_requests_total counter")
	m.requestsTotal.Range(func(k, v any) bool {
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
	fmt.Fprintf(w, "llmux_inflight_requests %d\n", m.Inflight())
	fmt.Fprintln(w, "# HELP llmux_upstream_errors_total Upstream provider errors.")
	fmt.Fprintln(w, "# TYPE llmux_upstream_errors_total counter")
	fmt.Fprintf(w, "llmux_upstream_errors_total %d\n", m.UpstreamErrors())
	fmt.Fprintln(w, "# HELP llmux_cache_hits_total Exact-cache hits.")
	fmt.Fprintln(w, "# TYPE llmux_cache_hits_total counter")
	fmt.Fprintf(w, "llmux_cache_hits_total %d\n", m.CacheHits())
	fmt.Fprintln(w, "# HELP llmux_egress_blocked_total Requests denied by the sovereignty gate (non-local provider without opt-in).")
	fmt.Fprintln(w, "# TYPE llmux_egress_blocked_total counter")
	fmt.Fprintf(w, "llmux_egress_blocked_total %d\n", m.EgressBlocked())
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

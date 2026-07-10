package cp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llmux/llmux/core/server"
)

// --- usageSpool unit tests ---------------------------------------------------

func TestUsageSpoolAddAckPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.json")

	sp, err := loadUsageSpool(path)
	if err != nil {
		t.Fatalf("loadUsageSpool (fresh): %v", err)
	}
	sp.add("k1", json.RawMessage(`{"a":1}`))
	sp.add("k2", json.RawMessage(`{"a":2}`))
	if got := sp.pending(); len(got) != 2 {
		t.Fatalf("pending=%v, want 2 entries", got)
	}

	// Persisted to disk: a fresh load sees both.
	sp2, err := loadUsageSpool(path)
	if err != nil {
		t.Fatalf("loadUsageSpool (reload): %v", err)
	}
	if got := sp2.pending(); len(got) != 2 {
		t.Fatalf("reloaded pending=%v, want 2 entries", got)
	}

	sp.ack("k1")
	if got := sp.pending(); len(got) != 1 {
		t.Fatalf("after ack, pending=%v, want 1 entry", got)
	}
	sp3, err := loadUsageSpool(path)
	if err != nil {
		t.Fatalf("loadUsageSpool (post-ack reload): %v", err)
	}
	got := sp3.pending()
	if len(got) != 1 {
		t.Fatalf("reloaded post-ack pending=%v, want 1 entry", got)
	}
	if _, ok := got["k2"]; !ok {
		t.Fatalf("expected k2 to survive ack of k1: %v", got)
	}
}

func TestUsageSpoolAddNoKeyIsNoop(t *testing.T) {
	dir := t.TempDir()
	sp, err := loadUsageSpool(filepath.Join(dir, "spool.json"))
	if err != nil {
		t.Fatalf("loadUsageSpool: %v", err)
	}
	sp.add("", json.RawMessage(`{"a":1}`))
	if got := sp.pending(); len(got) != 0 {
		t.Fatalf("expected no-op for empty key, got %v", got)
	}
}

func TestLoadUsageSpoolMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	sp, err := loadUsageSpool(filepath.Join(dir, "does-not-exist.json"))
	if err != nil {
		t.Fatalf("loadUsageSpool on missing file should not error: %v", err)
	}
	if got := sp.pending(); len(got) != 0 {
		t.Fatalf("expected empty spool, got %v", got)
	}
}

// --- UsageLogger + spool integration ----------------------------------------

// TestUsageSpoolSurvivesCrashRestart simulates the crash scenario the
// TODO(billing-reconcile) called out: a record was queued but the process
// died before cp acknowledged it. A NEW UsageLogger pointed at the same spool
// file must, on construction, pick the leftover record back up and deliver it
// — instead of that revenue record silently vanishing.
func TestUsageSpoolSurvivesCrashRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.json")

	// Simulate the pre-crash state directly: a record was spooled but never
	// acknowledged (as if the process died right after Log() wrote it).
	raw, _ := json.Marshal(usageBody{
		IdempotencyKey: "crash-key", Product: product, AccountID: "acct_crash",
		Kind: "llm_tokens", Count: 42, CostUSD: 0.02,
	})
	preCrash, err := loadUsageSpool(path)
	if err != nil {
		t.Fatalf("loadUsageSpool: %v", err)
	}
	preCrash.add("crash-key", raw)

	var got int32
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "crash-key" {
			atomic.AddInt32(&got, 1)
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}))
	defer srv.Close()

	// "Restart": a fresh UsageLogger pointed at the same spool file, with no
	// Log() call made on it — the only way the record can reach cp is via the
	// crash-recovery replay in NewUsageLogger.
	u := NewUsageLogger(New(srv.URL, "").WithUsageSpoolPath(path))
	t.Cleanup(u.Close)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("leftover spooled record from a simulated crash was never delivered (got=%d)", atomic.LoadInt32(&got))
	}
}

// TestUsageSpoolReconcilerDeliversAfterOutageOutlivesFastPath verifies the
// reconciler: a record that exhausts the fast path's usageMaxAttempts (cp
// down for longer than the fast path's bounded retries) is NOT lost — it
// stays in the spool and the background reconciler keeps retrying until cp
// recovers.
func TestUsageSpoolReconcilerDeliversAfterOutageOutlivesFastPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.json")

	var up int32 // 0 = cp down (fast path will exhaust attempts), 1 = cp up
	var hits int32
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&up) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Idempotency-Key") == "reconcile-key" {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, "").
		WithUsageSpoolPath(path).
		WithReconcileInterval(50 * time.Millisecond))
	t.Cleanup(u.Close)

	u.Log(server.UsageRecord{ID: "reconcile-key", AccountID: "acct_reconcile", Total: 7, CostUSD: 0.01})

	// Let the fast path exhaust all usageMaxAttempts against the down cp
	// (backoff caps at 5s per attempt in production, but the test's short
	// reconcile interval means the reconciler will pick it up independently
	// regardless of how far the fast path gets).
	time.Sleep(300 * time.Millisecond)

	// Bring cp "up" — only the reconciler (ticking every 50ms) should still be
	// trying at this point for a record whose fast-path attempts are long
	// exhausted, or the fast path itself if still mid-backoff. Either way it
	// must reach cp.
	atomic.StoreInt32(&up, 1)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("record never reached cp after recovery — reconciler failed to redeliver it")
	}

	// Once acknowledged, the spool must no longer carry it (poll briefly: the
	// ack that follows a successful POST is applied asynchronously by whichever
	// path delivered it).
	deadline := time.Now().Add(2 * time.Second)
	for {
		sp, err := loadUsageSpool(path)
		if err != nil {
			t.Fatalf("loadUsageSpool: %v", err)
		}
		if _, ok := sp.pending()["reconcile-key"]; !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("record still present in spool after being acknowledged by cp")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUsageSpoolQueueFullDropStillReachesCPViaReconciler verifies that a
// record dropped from the bounded in-memory fast-path queue (queue full) is
// still durably recoverable: Log() spools it BEFORE handing it to the
// channel, so even if enqueue() immediately discards it, the reconciler still
// delivers it.
func TestUsageSpoolQueueFullDropStillReachesCPViaReconciler(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.json")

	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	seen := map[string]bool{}
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-mu
		seen[r.Header.Get("Idempotency-Key")] = true
		found := seen["dropped-key"]
		mu <- struct{}{}
		if found {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}))
	defer srv.Close()

	u := NewUsageLogger(New(srv.URL, "").
		WithUsageSpoolPath(path).
		WithReconcileInterval(50 * time.Millisecond))
	t.Cleanup(u.Close)

	// Directly force a fast-path drop by filling the channel past its
	// capacity ourselves, bypassing Log's normal single-item enqueue, then
	// spool the record exactly as Log() would and enqueue it into the
	// already-full channel so enqueue()'s drop path fires deterministically.
	for i := 0; i < usageQueueDepth; i++ {
		u.ch <- usageItem{raw: json.RawMessage(`{}`), key: "filler"}
	}
	raw, _ := json.Marshal(usageBody{IdempotencyKey: "dropped-key", Product: product, AccountID: "acct_drop", Kind: "llm_tokens", Count: 1})
	u.sp.add("dropped-key", raw)
	u.enqueue(usageItem{raw: raw, key: "dropped-key"})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queue-full-dropped record was never delivered via the reconciler")
	}
}

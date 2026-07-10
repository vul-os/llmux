package cp

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// usageSpool durably persists the set of usage records handed to
// UsageLogger that cp has NOT yet acknowledged (2xx). It closes the
// durability gap flagged by TODO(billing-reconcile) in cp.go: the in-memory
// retry queue/channel is bounded (usageQueueDepth) and gives up after
// usageMaxAttempts, so a record that outlives either — or that never made it
// past a process crash — was previously lost unless the separate, ALSO
// optional, JSONL ledger (LLMUX_USAGE_LOG) happened to be configured.
//
// Every record is written here (keyed by its idempotency key) BEFORE it is
// handed to the in-memory delivery path (UsageLogger.enqueue), and removed
// only once cp actually acknowledges it. A background reconciler
// (UsageLogger.reconcileLoop) periodically re-POSTs everything still present
// here, so a record disappears if and only if cp has accepted it — an
// extended outage, a crash+restart, or a record dropped by the bounded queue
// all converge on this one durable backstop instead of three different
// (and, for the crash/drop cases, previously nonexistent) failure modes.
//
// The spool is opt-in (Config.UsageSpoolPath == "" disables it), matching the
// existing opt-in-persistence pattern used elsewhere in llmux (KeyStorePath,
// BYOK's StorePath): a self-hoster who hasn't configured a path keeps the
// historical in-memory-only behavior.
//
// The backing file is rewritten atomically (temp file + rename) on every
// change so a crash mid-write can never corrupt the existing, still-valid
// spool contents.
type usageSpool struct {
	path string

	// mu guards items: Log() (add), the worker goroutine, and the reconciler
	// goroutine (ack) all touch the spool concurrently.
	mu    sync.Mutex
	items map[string]json.RawMessage // idempotency key -> POST body
}

// loadUsageSpool opens (or, if absent, prepares to create) the spool file at
// path, loading any records left over from a previous run — e.g. ones that
// were queued but not yet acknowledged when the process crashed or was
// restarted.
func loadUsageSpool(path string) (*usageSpool, error) {
	sp := &usageSpool{path: path, items: map[string]json.RawMessage{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sp, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return sp, nil
	}
	if err := json.Unmarshal(data, &sp.items); err != nil {
		return nil, err
	}
	return sp, nil
}

// pending returns a snapshot of every currently un-acked record (idempotency
// key -> POST body).
func (s *usageSpool) pending() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]json.RawMessage, len(s.items))
	for k, v := range s.items {
		out[k] = v
	}
	return out
}

// add durably records a not-yet-acked usage POST body under key, BEFORE the
// caller hands the record to the in-memory delivery path — so the record
// survives even if the process crashes immediately after this call returns.
// A key of "" (no idempotency key on the source record) is a no-op: without a
// key there is nothing to track/dedupe durably, matching the existing
// idempotency-key-optional behavior of the POST path.
func (s *usageSpool) add(key string, raw json.RawMessage) {
	if key == "" {
		return
	}
	s.mu.Lock()
	s.items[key] = raw
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		log.Printf("cp: usage spool %s: persist failed (%v) — record %s stays only in the in-memory queue until the next successful spool write", s.path, err, key)
	}
}

// ack removes a record once cp has acknowledged it (2xx response).
func (s *usageSpool) ack(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.items[key]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.items, key)
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		log.Printf("cp: usage spool %s: persist failed after ack (%v) — %s may be re-sent (harmless: cp dedupes by Idempotency-Key)", s.path, err, key)
	}
}

// persistLocked rewrites the spool file to match s.items. Callers must hold
// s.mu. The temp-file + rename pattern (mirroring core/keys.FileStore.Flush)
// means readers never observe a partially-written file, and a crash between
// the WriteFile and the Rename leaves the ORIGINAL file intact.
func (s *usageSpool) persistLocked() error {
	data, err := json.Marshal(s.items)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

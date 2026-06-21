package keys

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/llmux/llmux/core/config"
)

// FileStore is a Store that persists cumulative spend to a JSON file so budgets
// survive restarts. Key definitions still come from config; rate-limit buckets
// stay in-memory (ephemeral). It is the dependency-free stand-in for a Postgres
// store — swap NewFileStore for a DB-backed Store without touching the gateway.
type FileStore struct {
	*memStore
	path  string
	mu    sync.Mutex
	dirty bool
}

// NewFileStore builds a file-backed store, loading any persisted spend.
func NewFileStore(cfgs []config.KeyConfig, path string) (*FileStore, error) {
	fs := &FileStore{memStore: newMemStore(cfgs), path: path}
	if err := fs.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return fs, nil
}

func (f *FileStore) load() error {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return err
	}
	var spend map[string]float64
	if err := json.Unmarshal(data, &spend); err != nil {
		return err
	}
	f.memStore.loadSpend(spend)
	return nil
}

// AddSpend records spend and marks the store dirty for the next flush.
func (f *FileStore) AddSpend(token string, usd float64) {
	f.memStore.AddSpend(token, usd)
	f.mu.Lock()
	f.dirty = true
	f.mu.Unlock()
}

// Flush atomically writes spend to disk if it changed since the last flush.
func (f *FileStore) Flush() error {
	f.mu.Lock()
	dirty := f.dirty
	f.dirty = false
	f.mu.Unlock()
	if !dirty {
		return nil
	}
	data, err := json.MarshalIndent(f.memStore.snapshotSpend(), "", "  ")
	if err != nil {
		f.markDirty()
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		f.markDirty()
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		f.markDirty()
		return err
	}
	return nil
}

func (f *FileStore) markDirty() {
	f.mu.Lock()
	f.dirty = true
	f.mu.Unlock()
}

// StartFlusher periodically flushes spend until ctx is done, then flushes once
// more on shutdown. Run it in a goroutine.
func (f *FileStore) StartFlusher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = f.Flush()
			return
		case <-t.C:
			_ = f.Flush()
		}
	}
}

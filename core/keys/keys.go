// Package keys implements virtual API keys with per-key budgets, rate limits,
// and model allow-lists. The Store interface allows swapping the in-memory
// backend for Postgres (Wave 5/ops) without touching the gateway.
package keys

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llmux/llmux/core/config"
)

// Key is a resolved virtual key with its limits and live counters.
type Key struct {
	Key           string
	Name          string
	BudgetUSD     float64
	RPM           int
	AllowedModels []string
}

// AllowsModel reports whether the key may use the given model.
func (k *Key) AllowsModel(model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, m := range k.AllowedModels {
		if m == model || m == "*" {
			return true
		}
	}
	return false
}

// Store manages keys, spend, and rate limiting.
type Store interface {
	// Lookup returns the key for a bearer token, or false if unknown.
	Lookup(token string) (*Key, bool)
	// Allow checks and consumes one unit of the per-minute rate limit.
	Allow(token string) bool
	// AddSpend records cost (USD) against a key.
	AddSpend(token string, usd float64)
	// Spend returns cumulative spend for a key.
	Spend(token string) float64
	// OverBudget reports whether the key has exhausted its budget.
	OverBudget(token string) bool
	// Keys returns all configured keys (for admin listing).
	Keys() []*Key
}

// memStore is an in-memory Store with a token-bucket rate limiter. Spend is
// tracked in integer micro-dollars (USD×1e6) to avoid float accumulation drift
// across many sub-cent charges.
//
// The keys, spend, and buckets maps are built once in newMemStore and never
// structurally written again, so they are read without any lock. Only the
// per-key counters mutate: spend via sync/atomic (*int64 micro-dollars), and
// each bucket under its own mutex. This keeps the request hot path lock-free at
// the store level.
type memStore struct {
	keys    map[string]*Key
	spend   map[string]*int64 // micro-dollars, mutated atomically
	buckets map[string]*bucket
	now     func() time.Time
}

func usdToMicro(usd float64) int64   { return int64(math.Round(usd * 1e6)) }
func microToUSD(micro int64) float64 { return float64(micro) / 1e6 }

// NewMemStore builds an in-memory store from static key configs.
func NewMemStore(cfgs []config.KeyConfig) Store { return newMemStore(cfgs) }

// newMemStore builds the concrete in-memory store (used by FileStore too). It
// pre-creates a spend counter for every configured key (so the spend map is
// structurally read-only) and a rate-limit bucket for every key with RPM>0 (so
// the buckets map is structurally read-only too).
func newMemStore(cfgs []config.KeyConfig) *memStore {
	s := &memStore{
		keys:    map[string]*Key{},
		spend:   map[string]*int64{},
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
	for _, c := range cfgs {
		s.keys[c.Key] = &Key{
			Key: c.Key, Name: c.Name, BudgetUSD: c.BudgetUSD, RPM: c.RPM,
			AllowedModels: c.AllowedModels,
		}
		s.spend[c.Key] = new(int64)
		if c.RPM > 0 {
			s.buckets[c.Key] = newBucket(float64(c.RPM), float64(c.RPM)/60.0)
		}
	}
	return s
}

// Keys returns all configured keys. The keys map is read-only after construction.
func (s *memStore) Keys() []*Key {
	out := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	return out
}

// snapshotSpend returns a copy of the spend map.
func (s *memStore) snapshotSpend() map[string]float64 {
	out := make(map[string]float64, len(s.spend))
	for k, p := range s.spend {
		out[k] = microToUSD(atomic.LoadInt64(p))
	}
	return out
}

// loadSpend merges persisted spend (USD) into the store. Counters for known
// keys are set atomically; persisted entries for keys no longer configured are
// ignored (the spend map is structurally fixed at construction).
func (s *memStore) loadSpend(m map[string]float64) {
	for k, v := range m {
		if p, ok := s.spend[k]; ok {
			atomic.StoreInt64(p, usdToMicro(v))
		}
	}
}

func (s *memStore) Lookup(token string) (*Key, bool) {
	k, ok := s.keys[token]
	return k, ok
}

func (s *memStore) Allow(token string) bool {
	k, ok := s.keys[token]
	if !ok || k.RPM <= 0 {
		return true // unknown or unlimited
	}
	b := s.buckets[token]
	if b == nil {
		return true // no bucket pre-created (e.g. RPM<=0); treat as unlimited
	}
	return b.take(s.now())
}

func (s *memStore) AddSpend(token string, usd float64) {
	if p, ok := s.spend[token]; ok {
		atomic.AddInt64(p, usdToMicro(usd))
	}
}

func (s *memStore) Spend(token string) float64 {
	if p, ok := s.spend[token]; ok {
		return microToUSD(atomic.LoadInt64(p))
	}
	return 0
}

func (s *memStore) OverBudget(token string) bool {
	k, ok := s.keys[token]
	if !ok || k.BudgetUSD <= 0 {
		return false
	}
	p, ok := s.spend[token]
	if !ok {
		return false
	}
	return atomic.LoadInt64(p) >= usdToMicro(k.BudgetUSD)
}

// bucket is a token-bucket rate limiter. Each bucket carries its own mutex so
// callers can take from different buckets concurrently without a shared lock.
type bucket struct {
	mu       sync.Mutex
	tokens   float64
	capacity float64
	rate     float64 // tokens per second
	last     time.Time
}

func newBucket(capacity, ratePerSec float64) *bucket {
	return &bucket{tokens: capacity, capacity: capacity, rate: ratePerSec}
}

func (b *bucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last = now
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

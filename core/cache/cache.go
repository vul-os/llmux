// Package cache provides response caching. The exact-match cache keys on a hash
// of the request and serves identical repeated requests without an upstream
// call. Semantic caching (embedding similarity) plugs in behind the same Cache
// interface later.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/llmux/llmux/core/openai"
)

// Entry is a cached response: the pre-serialized JSON body plus its usage. The
// body is stored once and written verbatim on a hit (no re-marshal, no shared
// mutable response pointer); Usage lets the handler attribute cost/usage on a
// hit without re-parsing.
type Entry struct {
	Body  []byte
	Usage *openai.Usage
}

// Cache stores and retrieves cached response entries by key.
type Cache interface {
	Get(key string) (*Entry, bool)
	Set(key string, e *Entry)
}

// KeyFor derives a stable cache key from the raw request body.
func KeyFor(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type lruItem struct {
	key     string
	entry   *Entry
	expires time.Time
}

// LRU is an in-memory, size-bounded, TTL'd exact-match cache.
type LRU struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	ll    *list.List
	items map[string]*list.Element
	now   func() time.Time
}

// NewLRU builds an LRU cache. maxEntries<=0 defaults to 10000.
func NewLRU(maxEntries int, ttl time.Duration) *LRU {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &LRU{
		max: maxEntries, ttl: ttl,
		ll: list.New(), items: map[string]*list.Element{}, now: time.Now,
	}
}

// Get returns a cached entry if present and unexpired.
func (c *LRU) Get(key string) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*lruItem)
	if !it.expires.IsZero() && c.now().After(it.expires) {
		c.removeElement(el)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return it.entry, true
}

// Set stores an entry, evicting the least-recently-used one if needed.
func (c *LRU) Set(key string, e *Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*lruItem)
		it.entry = e
		it.expires = c.expiry()
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&lruItem{key: key, entry: e, expires: c.expiry()})
	c.items[key] = el
	for c.ll.Len() > c.max {
		c.removeElement(c.ll.Back())
	}
}

func (c *LRU) expiry() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return c.now().Add(c.ttl)
}

func (c *LRU) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.items, el.Value.(*lruItem).key)
}

// Len returns the current number of cached entries.
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

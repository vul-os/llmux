package cache

import (
	"context"
	"math"
	"sync"
	"time"
)

// Embedder turns text into a fixed-length embedding vector. Implementations are
// expected to be deterministic for a given input so that semantically identical
// requests map to nearby vectors.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

// semanticEntry holds a stored response keyed by its embedding rather than an
// exact text/hash key. expires is zero when the entry never expires.
type semanticEntry struct {
	vec     []float64
	entry   *Entry
	expires time.Time
}

// SemanticCache serves cached chat completion responses when an incoming
// request is sufficiently similar (cosine similarity >= threshold) to a stored
// one. It implements the Cache interface; the key passed to Get/Set is the
// canonical request TEXT (not a hash), which is embedded via the Embedder.
//
// It is safe for concurrent use. On any embedder error it degrades gracefully:
// Get returns a miss and Set becomes a no-op.
type SemanticCache struct {
	mu        sync.Mutex
	embedder  Embedder
	threshold float64
	max       int
	ttl       time.Duration
	entries   []semanticEntry // ordered oldest-first; FIFO eviction
	now       func() time.Time
}

// NewSemanticCache builds a SemanticCache. threshold is the minimum cosine
// similarity (e.g. 0.95) required for a hit. maxEntries<=0 defaults to 10000.
// A non-positive ttl disables expiry.
func NewSemanticCache(embedder Embedder, threshold float64, maxEntries int, ttl time.Duration) *SemanticCache {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &SemanticCache{
		embedder:  embedder,
		threshold: threshold,
		max:       maxEntries,
		ttl:       ttl,
		now:       time.Now,
	}
}

// Get embeds the key text and returns the stored response with the highest
// cosine similarity, provided it meets the threshold and has not expired.
// Expired entries encountered during the scan are pruned. On embedder error it
// returns (nil, false).
func (c *SemanticCache) Get(key string) (*Entry, bool) {
	vec, err := c.embedder.Embed(context.Background(), key)
	if err != nil {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	bestIdx := -1
	bestSim := c.threshold
	kept := c.entries[:0]
	for i := range c.entries {
		en := c.entries[i]
		if !en.expires.IsZero() && now.After(en.expires) {
			continue // drop expired entry
		}
		idx := len(kept)
		kept = append(kept, en)
		if sim := cosine(vec, en.vec); sim >= bestSim {
			bestSim = sim
			bestIdx = idx
		}
	}
	// Clear tail references freed by compaction so responses can be GC'd.
	for i := len(kept); i < len(c.entries); i++ {
		c.entries[i] = semanticEntry{}
	}
	c.entries = kept

	if bestIdx < 0 {
		return nil, false
	}
	return c.entries[bestIdx].entry, true
}

// Set embeds the key text and stores the response. Oldest entries are evicted
// (FIFO) once capacity is exceeded. On embedder error it is a no-op.
func (c *SemanticCache) Set(key string, e *Entry) {
	vec, err := c.embedder.Embed(context.Background(), key)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = append(c.entries, semanticEntry{
		vec:     vec,
		entry:   e,
		expires: c.expiry(),
	})
	if n := len(c.entries) - c.max; n > 0 {
		// Drop the n oldest entries.
		copy(c.entries, c.entries[n:])
		for i := len(c.entries) - n; i < len(c.entries); i++ {
			c.entries[i] = semanticEntry{}
		}
		c.entries = c.entries[:len(c.entries)-n]
	}
}

func (c *SemanticCache) expiry() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return c.now().Add(c.ttl)
}

// Len returns the current number of stored entries (including any not yet
// pruned expired ones).
func (c *SemanticCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// cosine returns the cosine similarity between a and b. It returns 0 for
// mismatched lengths or when either vector has zero magnitude.
func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

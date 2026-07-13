package keys

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// MemLimiter is an in-memory token-bucket Limiter (single-instance).
type MemLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

// NewMemLimiter builds an in-memory limiter.
func NewMemLimiter() *MemLimiter {
	return &MemLimiter{buckets: map[string]*bucket{}, now: time.Now}
}

// Allow implements Limiter.
func (m *MemLimiter) Allow(token string, rpm int) bool {
	if rpm <= 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[token]
	if b == nil {
		b = newBucket(float64(rpm), float64(rpm)/60.0)
		m.buckets[token] = b
	}
	return b.take(m.now())
}

// RedisLimiter is a cross-replica fixed-window Limiter backed by Redis.
type RedisLimiter struct {
	rdb *redis.Client
	// local is the strict in-process cap enforced while Redis is unreachable, so
	// an outage degrades the limit from fleet-wide to per-replica rather than
	// removing it.
	local *MemLimiter

	warnMu sync.Mutex
	warnAt time.Time
}

// NewRedisLimiter builds a Redis-backed limiter from an existing client.
func NewRedisLimiter(rdb *redis.Client) *RedisLimiter {
	return &RedisLimiter{rdb: rdb, local: NewMemLimiter()}
}

// redisOutageLogInterval throttles the outage warning so a sustained Redis
// outage cannot flood the log at request rate.
const redisOutageLogInterval = time.Minute

// Allow implements Limiter using a per-minute fixed window (INCR + EXPIRE).
//
// FAIL CLOSED (degraded): a Redis error does NOT wave the request through — it
// is held to a strict in-process token bucket at the same RPM. An outage
// degrades the cap from fleet-wide to per-replica instead of lifting every
// per-key RPM cap at once, which matters because a key with BudgetUSD<=0 has RPM
// as its ONLY throttle against the operator's real provider keys.
// The Redis key uses sha256(token) instead of the raw bearer token so that a
// Redis SCAN/MONITOR never exposes live credentials.
func (r *RedisLimiter) Allow(token string, rpm int) bool {
	if rpm <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	window := time.Now().Unix() / 60
	key := fmt.Sprintf("llmux:rl:%s:%d", HashToken(token), window)
	n, err := r.rdb.Incr(ctx, key).Result()
	if err != nil {
		r.warnOutage(err)
		return r.local.Allow(token, rpm)
	}
	if n == 1 {
		_ = r.rdb.Expire(ctx, key, 65*time.Second).Err()
	}
	return n <= int64(rpm)
}

// warnOutage logs a Redis outage at most once per redisOutageLogInterval.
func (r *RedisLimiter) warnOutage(err error) {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if time.Since(r.warnAt) < redisOutageLogInterval {
		return
	}
	r.warnAt = time.Now()
	log.Printf("keys: redis rate limiter unavailable (%v) — degrading to a strict per-replica RPM cap", err)
}

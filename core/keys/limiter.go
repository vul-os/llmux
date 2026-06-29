package keys

import (
	"context"
	"fmt"
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
}

// NewRedisLimiter builds a Redis-backed limiter from an existing client.
func NewRedisLimiter(rdb *redis.Client) *RedisLimiter { return &RedisLimiter{rdb: rdb} }

// Allow implements Limiter using a per-minute fixed window (INCR + EXPIRE).
// On Redis error it fails open (allows) so a Redis outage never hard-blocks traffic.
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
		return true // fail open
	}
	if n == 1 {
		_ = r.rdb.Expire(ctx, key, 65*time.Second).Err()
	}
	return n <= int64(rpm)
}

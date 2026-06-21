package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache is a cross-replica exact-match response cache backed by Redis.
// It implements Cache, so it drops in wherever the LRU is used.
type RedisCache struct {
	rdb    *redis.Client
	ttl    time.Duration
	prefix string
}

// NewRedisCache builds a Redis cache from an existing client. ttl<=0 means no
// expiry (not recommended for a shared cache).
func NewRedisCache(rdb *redis.Client, ttl time.Duration) *RedisCache {
	return &RedisCache{rdb: rdb, ttl: ttl, prefix: "llmux:cache:"}
}

// Get implements Cache. Misses and Redis errors both return (nil,false) so a
// cache outage degrades to a normal upstream call.
func (c *RedisCache) Get(key string) (*Entry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := c.rdb.Get(ctx, c.prefix+key).Bytes()
	if err != nil {
		return nil, false
	}
	var e Entry
	if json.Unmarshal(data, &e) != nil {
		return nil, false
	}
	return &e, true
}

// Set implements Cache.
func (c *RedisCache) Set(key string, e *Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.rdb.Set(ctx, c.prefix+key, data, c.ttl).Err()
}

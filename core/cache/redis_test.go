package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/llmux/llmux/core/openai"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	addr := os.Getenv("LLMUX_TEST_REDIS")
	if addr == "" {
		t.Skip("set LLMUX_TEST_REDIS to run Redis cache integration tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	return rdb
}

func TestRedisCacheRoundTrip(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.FlushDB(context.Background())
	c := NewRedisCache(rdb, time.Minute)

	if _, ok := c.Get("missing"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set("k1", &Entry{Body: []byte(`{"id":"abc"}`), Usage: &openai.Usage{TotalTokens: 7}})
	got, ok := c.Get("k1")
	if !ok || string(got.Body) != `{"id":"abc"}` || got.Usage.TotalTokens != 7 {
		t.Fatalf("round-trip failed: %+v ok=%v", got, ok)
	}
}

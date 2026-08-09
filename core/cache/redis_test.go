package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vul-os/llmux/core/openai"
	"github.com/redis/go-redis/v9"
)

// testRedis is the Redis integration gate. It records the outcome (see
// integration_gate_test.go) so a skipped run says out loud what it did not
// verify instead of passing quietly.
func testRedis(t *testing.T) *redis.Client {
	addr := os.Getenv(redisEnv)
	if addr == "" {
		gateRecord(t.Name(), false)
		t.Skip("set " + redisEnv + " to run Redis cache integration tests (the SHARED, cross-replica cache " +
			"path is NOT verified without it)")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		gateRecord(t.Name(), false)
		t.Skipf("redis at %s not reachable (%v) — the shared cache path is NOT verified", addr, err)
	}
	gateRecord(t.Name(), true)
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

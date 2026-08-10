package gateway

// Close used to make itself idempotent by setting g.rdb = nil. That is an
// unsynchronised write to a field Start reads (its Redis ping) and a second
// Close also writes — a data race `go test -race` reports, and one with a real
// failure mode beyond the report: Close could null the field between Start's
// `g.rdb != nil` check and its `g.rdb.Ping`, turning a shutdown into a nil
// dereference in the host's process.
//
// Idempotence is sync.Once's job now and the field is immutable after New.
// These tests are only meaningful under -race; `make test` runs the whole root
// module that way.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vul-os/llmux/core/config"
)

func redisConfiguredGateway(t *testing.T) *Gateway {
	t.Helper()
	g, err := New(&config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		// A port nothing listens on: the client is built (which is all the race
		// needs — it makes g.rdb non-nil) and any ping fails fast.
		Redis: "127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	if g.rdb == nil {
		t.Fatal("no Redis client was built, so the field this test is about is nil and the " +
			"assertions below touch nothing")
	}
	return g
}

func TestConcurrentClosesDoNotRaceOnTheRedisClient(t *testing.T) {
	for i := 0; i < 40; i++ {
		g := redisConfiguredGateway(t)
		var wg sync.WaitGroup
		wg.Add(2)
		for j := 0; j < 2; j++ {
			go func() {
				defer wg.Done()
				_ = g.Close()
			}()
		}
		wg.Wait()
	}
}

func TestCloseDoesNotRaceAgainstStart(t *testing.T) {
	for i := 0; i < 40; i++ {
		g := redisConfiguredGateway(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_ = g.Start(ctx) // reads g.rdb, then pings it
		}()
		go func() {
			defer wg.Done()
			_ = g.Close()
		}()
		wg.Wait()
	}
}

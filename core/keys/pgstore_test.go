package keys

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/llmux/llmux/core/config"
	"github.com/redis/go-redis/v9"
)

func testDSN(t *testing.T) string {
	dsn := os.Getenv("LLMUX_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set LLMUX_TEST_POSTGRES to run Postgres integration tests")
	}
	return dsn
}

// TestPGStorePersistsAcrossInstances proves budgets/spend survive a restart and
// are shared by another instance (the multi-replica correctness property).
func TestPGStorePersistsAcrossInstances(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	// Clean slate.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, "DROP TABLE IF EXISTS llmux_keys")
	pool.Close()

	cfgs := []config.KeyConfig{{Key: "sk-pg", Name: "team", BudgetUSD: 1.0, AllowedModels: []string{"gpt-4o"}}}
	s1, err := NewPGStore(ctx, dsn, cfgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	if k, ok := s1.Lookup("sk-pg"); !ok || k.Name != "team" || !k.AllowsModel("gpt-4o") {
		t.Fatalf("lookup/seed failed: %+v", k)
	}
	if s1.OverBudget("sk-pg") {
		t.Fatal("should start under budget")
	}
	s1.AddSpend("sk-pg", 0.6)
	if s1.OverBudget("sk-pg") {
		t.Fatal("0.6 < 1.0 should be under budget")
	}
	s1.AddSpend("sk-pg", 0.6) // total 1.2

	// A second instance (simulating another replica / restart) sees the spend.
	s2, err := NewPGStore(ctx, dsn, cfgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Spend("sk-pg"); got < 1.19 || got > 1.21 {
		t.Fatalf("cross-instance spend=%v, want ~1.2", got)
	}
	if !s2.OverBudget("sk-pg") {
		t.Fatal("second instance must see over-budget")
	}
	if len(s2.Keys()) < 1 {
		t.Fatal("Keys() empty")
	}
}

func testRedisClient(t *testing.T) *redis.Client {
	addr := os.Getenv("LLMUX_TEST_REDIS")
	if addr == "" {
		t.Skip("set LLMUX_TEST_REDIS to run Redis integration tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	return rdb
}

func TestRedisLimiterFixedWindow(t *testing.T) {
	rdb := testRedisClient(t)
	defer rdb.FlushDB(context.Background())
	lim := NewRedisLimiter(rdb)
	tok := fmt.Sprintf("tok-%d", time.Now().UnixNano())

	if !lim.Allow(tok, 2) || !lim.Allow(tok, 2) {
		t.Fatal("first two requests should pass (rpm=2)")
	}
	if lim.Allow(tok, 2) {
		t.Fatal("third request in window should be denied")
	}
}

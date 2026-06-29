package keys

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// TestPGStoreKeyHashedAtRest verifies the security property: the raw bearer
// token is NEVER stored as the Postgres "key" column value. The column must
// contain sha256(rawToken) so a PG dump never yields live credentials.
func TestPGStoreKeyHashedAtRest(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool.Exec(ctx, "DROP TABLE IF EXISTS llmux_keys")
	pool.Close()

	const rawToken = "sk-at-rest-secret"
	cfgs := []config.KeyConfig{{Key: rawToken, Name: "atrest", BudgetUSD: 1.0}}
	s, err := NewPGStore(ctx, dsn, cfgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Lookup must still work (validates via hash).
	k, ok := s.Lookup(rawToken)
	if !ok || k.Name != "atrest" {
		t.Fatalf("Lookup after hash-seed failed: ok=%v k=%+v", ok, k)
	}

	// Directly inspect the Postgres row: the "key" column must hold the hash,
	// not the raw token.
	p2, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	rows, err := p2.Query(ctx, "SELECT key FROM llmux_keys")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored == rawToken {
			t.Fatalf("raw token found in Postgres key column: %q", stored)
		}
		if strings.Contains(stored, rawToken) {
			t.Fatalf("raw token appears inside stored key column value: %q", stored)
		}
		wantHash := HashToken(rawToken)
		if stored != wantHash {
			t.Fatalf("stored key = %q, want hash %q", stored, wantHash)
		}
	}
}

// TestRedisLimiterKeyHashedAtRest verifies that the Redis rate-limit key
// contains the sha256 hash of the token, not the raw token itself.
func TestRedisLimiterKeyHashedAtRest(t *testing.T) {
	rdb := testRedisClient(t)
	defer rdb.FlushDB(context.Background())
	lim := NewRedisLimiter(rdb)
	const rawToken = "sk-redis-secret"

	// Trigger a rate-limit entry.
	lim.Allow(rawToken, 100)

	// SCAN for all keys; none should contain the raw token.
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.Contains(k, rawToken) {
			t.Fatalf("raw token found in Redis key: %q", k)
		}
		// The key must contain the hash instead.
		if !strings.Contains(k, HashToken(rawToken)) {
			t.Fatalf("Redis key does not contain expected hash: %q", k)
		}
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

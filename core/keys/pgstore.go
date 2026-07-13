package keys

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/llmux/llmux/core/config"
)

// DefaultSchema is the Postgres schema llmux uses when none is configured. A
// dedicated schema lets llmux share one database (e.g. a single Neon database)
// with the other Vulos products without table-name collisions.
const DefaultSchema = "llmux"

// PGStore is a Postgres-backed Store: key definitions + cumulative spend live in
// Postgres, so budgets are correct across replicas. Rate limiting is delegated
// to a Limiter (Redis-backed for cross-replica correctness, or in-memory).
// Key definitions are seeded from config and cached in memory for fast lookup.
type PGStore struct {
	pool    *pgxpool.Pool
	limiter Limiter

	// schema is the Postgres schema holding llmux's tables (default "llmux").
	schema string
	// table is the fully-qualified, sanitized table identifier (schema.keys).
	table string

	mu   sync.RWMutex
	keys map[string]*Key

	// spendMu guards spend, the degraded-mode bookkeeping consulted only when
	// Postgres cannot answer.
	spendMu sync.Mutex
	spend   map[string]*spendState
}

// spendState is the degraded-mode bookkeeping for one key: the spend Postgres is
// known to hold, plus spend Postgres has not durably accepted. It mirrors the cp
// gate's last-known-good posture (integration/cp): an outage falls back to the
// last known figure, never to "unspent".
type spendState struct {
	// db is the last spend_usd Postgres was seen to hold. Valid only when known.
	db float64
	// known is true once a read has succeeded for the key. Without it there is no
	// basis at all to judge the budget, and OverBudget must fail closed.
	known bool
	// pending is spend a failed write left unpersisted. It counts against the
	// budget immediately and is folded into the next write attempt, so a DB blip
	// never silently forgives real spend.
	pending float64
}

// Limiter enforces a per-minute request limit for a token.
type Limiter interface {
	Allow(token string, rpm int) bool
}

// NewPGStore connects, migrates, seeds keys from config, and returns a store.
// limiter may be nil (defaults to an in-memory token-bucket limiter). schema is
// the Postgres schema to hold llmux's tables; empty defaults to DefaultSchema
// ("llmux") so llmux can share one database with other Vulos products.
func NewPGStore(ctx context.Context, dsn, schema string, cfgs []config.KeyConfig, limiter Limiter) (*PGStore, error) {
	if schema == "" {
		schema = DefaultSchema
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	if limiter == nil {
		limiter = NewMemLimiter()
	}
	// Sanitize the schema/table as SQL identifiers (defends against injection
	// and quotes mixed-case/reserved names) since they are interpolated into DDL
	// and DML strings rather than passed as parameters.
	table := pgx.Identifier{schema, "llmux_keys"}.Sanitize()
	s := &PGStore{pool: pool, limiter: limiter, schema: schema, table: table,
		keys: map[string]*Key{}, spend: map[string]*spendState{}}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := s.seed(ctx, cfgs); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *PGStore) Close() { s.pool.Close() }

func (s *PGStore) migrate(ctx context.Context) error {
	// Create the dedicated schema first so the table can live under it on a
	// database shared with other products. CREATE SCHEMA/TABLE IF NOT EXISTS are
	// idempotent, so this is safe to run on every startup.
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, pgx.Identifier{s.schema}.Sanitize())); err != nil {
		return fmt.Errorf("create schema %q: %w", s.schema, err)
	}
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
  key            TEXT PRIMARY KEY,
  name           TEXT NOT NULL DEFAULT '',
  budget_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
  rpm            INTEGER NOT NULL DEFAULT 0,
  allowed_models TEXT[] NOT NULL DEFAULT '{}',
  spend_usd      DOUBLE PRECISION NOT NULL DEFAULT 0,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);`, s.table))
	return err
}

// seed upserts config keys (preserving existing spend) and caches them.
// The Postgres "key" column stores sha256(rawToken) so that a PG dump never
// exposes live bearer credentials. The in-memory map is keyed by the raw
// token for fast O(1) Lookup; DB operations hash on the fly.
func (s *PGStore) seed(ctx context.Context, cfgs []config.KeyConfig) error {
	for _, c := range cfgs {
		models := c.AllowedModels
		if models == nil {
			models = []string{} // NOT NULL array column
		}
		h := HashToken(c.Key)
		_, err := s.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s (key, name, budget_usd, rpm, allowed_models)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (key) DO UPDATE SET
  name=EXCLUDED.name, budget_usd=EXCLUDED.budget_usd,
  rpm=EXCLUDED.rpm, allowed_models=EXCLUDED.allowed_models`, s.table),
			h, c.Name, c.BudgetUSD, c.RPM, models)
		if err != nil {
			return fmt.Errorf("seed key: %w", err)
		}
		// Populate the in-memory cache with the raw token as the map key.
		// Key.Key holds the raw token so callers (admin listing, cacheScope, etc.)
		// always deal in raw tokens; only DB/Redis paths hash.
		s.keys[c.Key] = &Key{
			Key: c.Key, Name: c.Name, BudgetUSD: c.BudgetUSD,
			RPM: c.RPM, AllowedModels: models,
		}
	}
	return nil
}

// Lookup implements Store (from the in-memory key cache).
func (s *PGStore) Lookup(token string) (*Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[token]
	return k, ok
}

// Keys implements Store.
func (s *PGStore) Keys() []*Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	return out
}

// Allow implements Store via the configured limiter.
func (s *PGStore) Allow(token string) bool {
	k, ok := s.Lookup(token)
	if !ok || k.RPM <= 0 {
		return true
	}
	return s.limiter.Allow(token, k.RPM)
}

// AddSpend implements Store (atomic increment in Postgres).
// token is the raw bearer token; it is hashed before the DB UPDATE so the
// plaintext credential is never written to the spend row.
//
// A failed write is never swallowed: the amount, plus anything an earlier write
// failed to persist, is held as pending spend — counted against the budget from
// that moment and folded into the next write attempt. A write that fails
// ambiguously (e.g. a timeout after Postgres applied it) can therefore be
// counted twice; over-counting spend is the safe side of a budget.
func (s *PGStore) AddSpend(token string, usd float64) {
	amount := usd + s.takePending(token)
	if amount == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET spend_usd = spend_usd + $2 WHERE key=$1`, s.table), HashToken(token), amount)
	if err != nil {
		s.holdPending(token, amount)
		log.Printf("keys: postgres spend write failed for key %q (%v) — holding $%.4f as pending spend, enforced locally until it persists", s.nameFor(token), err, amount)
		return
	}
	if tag.RowsAffected() == 0 {
		// No row for this key (never seeded, or deleted): there is nothing to
		// accumulate against, so holding the amount forever would be pointless.
		return
	}
	s.applyPersisted(token, amount)
}

// Spend implements Store.
// token is the raw bearer token; it is hashed before the DB SELECT.
// When Postgres cannot be read it reports the last-known-good figure; see spendUSD.
func (s *PGStore) Spend(token string) float64 {
	usd, _ := s.spendUSD(token)
	return usd
}

// OverBudget implements Store.
//
// FAIL CLOSED: when Postgres cannot be read and no last-known-good spend exists
// for the key, its spend is unknowable, so the key is reported OVER budget
// rather than unspent. Discarding the error here would hand every virtual key —
// including already-exhausted ones — an unbounded budget against the operator's
// real provider keys for the duration of a single DB blip.
func (s *PGStore) OverBudget(token string) bool {
	k, ok := s.Lookup(token)
	if !ok || k.BudgetUSD <= 0 {
		return false
	}
	usd, known := s.spendUSD(token)
	if !known {
		return true
	}
	return usd >= k.BudgetUSD
}

// spendUSD returns the key's effective spend: the Postgres figure plus any spend
// still pending locally. known is false only when Postgres could not be read AND
// no last-known-good figure exists — the caller must then fail closed instead of
// treating the key as unspent.
func (s *PGStore) spendUSD(token string) (float64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v float64
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT spend_usd FROM %s WHERE key=$1`, s.table), HashToken(token)).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s.degradedSpend(token, err)
	}
	// ErrNoRows means the key has no spend row: a definitive $0, not an outage.
	return s.rememberSpend(token, v), true
}

// rememberSpend caches a freshly-read Postgres figure as the last-known-good
// spend and returns it plus any spend still pending locally.
func (s *PGStore) rememberSpend(token string, v float64) float64 {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	st := s.stateLocked(token)
	st.db, st.known = v, true
	return v + st.pending
}

// degradedSpend answers a spend query while Postgres is unreadable: it enforces
// the last-known-good figure (plus unpersisted spend) when there is one, and
// reports "unknown" when there is not, so the budget check can fail closed.
func (s *PGStore) degradedSpend(token string, err error) (float64, bool) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	st := s.spend[token]
	if st == nil || !st.known {
		log.Printf("keys: postgres spend read failed for key %q (%v) — no last-known-good spend, failing closed", s.nameFor(token), err)
		return 0, false
	}
	usd := st.db + st.pending
	log.Printf("keys: postgres spend read failed for key %q (%v) — enforcing last-known-good spend $%.4f", s.nameFor(token), err, usd)
	return usd, true
}

// takePending removes and returns the key's unpersisted spend, so a write can
// fold it in. It is put back (holdPending) if that write also fails.
func (s *PGStore) takePending(token string) float64 {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	st := s.spend[token]
	if st == nil {
		return 0
	}
	v := st.pending
	st.pending = 0
	return v
}

// holdPending records spend Postgres would not accept.
func (s *PGStore) holdPending(token string, usd float64) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.stateLocked(token).pending += usd
}

// applyPersisted advances the last-known-good figure after a successful write so
// it stays usable if Postgres later goes away.
func (s *PGStore) applyPersisted(token string, usd float64) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	if st := s.stateLocked(token); st.known {
		st.db += usd
	}
}

// stateLocked returns (creating if needed) the spend bookkeeping for a token.
// The caller holds spendMu.
func (s *PGStore) stateLocked(token string) *spendState {
	st := s.spend[token]
	if st == nil {
		st = &spendState{}
		s.spend[token] = st
	}
	return st
}

// nameFor returns the configured name for a token, for logs. The raw bearer
// token is never logged.
func (s *PGStore) nameFor(token string) string {
	if k, ok := s.Lookup(token); ok {
		return k.Name
	}
	return "unknown"
}

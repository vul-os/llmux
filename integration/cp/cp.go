// Package cp is the OPTIONAL Vulos control-plane ("cp" / vulos-cloud) adapter
// for the llmux integration seam.
//
// It implements the server.Identity / server.BudgetGate / server.UsageLogger
// interfaces against the control plane. It is a SEPARATE package on purpose:
//
//   - The llmux core gateway never imports it (verify: `go list -deps` on the
//     core packages shows no integration/cp). Only the composition root
//     (cmd/llmux/main.go) references it, and only when LLMUX_CP_URL is set.
//   - Deleting this package must not break the standalone build. The core falls
//     back to its static-key defaults (server.staticIdentity / staticBudgetGate).
//
// All cp calls authenticate with the X-Relay-Auth header carrying the shared
// secret. The request/response shapes match the cp contract shared across the
// Vulos products that consume the control plane.
package cp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/llmux/llmux/core/server"
)

// HeaderRelayAuth is the shared cp authentication header. Its value is the
// configured shared secret (LLMUX_CP_SECRET / cp_shared_secret).
const HeaderRelayAuth = "X-Relay-Auth"

// product is the cp product identifier for llmux usage/entitlements.
const product = "llm"

// Config holds the resolved cp adapter settings.
type Config struct {
	// BaseURL is the control-plane base URL (trailing slash trimmed).
	BaseURL string
	// Secret is the shared secret sent as X-Relay-Auth on every cp call.
	Secret string
	// RPM is the per-account requests-per-minute cap for cp principals (0 = off).
	RPM int
	// EntitlementTTL bounds how long a fetched entitlement is cached/reused.
	EntitlementTTL time.Duration
	// DegradedFailOpen, when true, allows a request with NO bound when cp is
	// unreachable and nothing is cached (cold cache). Default false: cold-cache
	// degraded mode is bounded by DegradedRPM instead of failing fully open.
	DegradedFailOpen bool
	// DegradedRPM is the conservative per-account RPM cap enforced ONLY in
	// cold-cache degraded mode when DegradedFailOpen is false. 0 = a built-in
	// conservative default (defaultDegradedRPM).
	DegradedRPM int
	// IdentityCacheMax caps the number of last-known-good identity entries
	// retained. The cache is pruned lazily on insert (expired entries swept,
	// oldest evicted past the cap) so it stays bounded by distinct-token churn.
	// 0 = a built-in default (defaultIdentityCacheMax).
	IdentityCacheMax int
	// UsageSpoolPath, if set, durably persists the UsageLogger's pending
	// (not-yet-acked) usage records to this file so they survive a process
	// restart or crash instead of relying solely on the bounded in-memory
	// retry queue. See usageSpool. Empty = no spool (the historical
	// in-memory-only behavior).
	UsageSpoolPath string
	// ReconcileInterval controls how often the background reconciler (started
	// only when UsageSpoolPath is set) retries every still-un-acked spooled
	// usage record. 0 selects a built-in default (defaultReconcileInterval).
	ReconcileInterval time.Duration
}

// Enabled reports whether the cp adapter should be wired (a base URL is set).
func (c Config) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

// New builds a Config, normalizing the base URL.
func New(baseURL, secret string) Config {
	return Config{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Secret:  strings.TrimSpace(secret),
	}
}

// WithRPM returns a copy of the config with the per-account RPM cap set.
func (c Config) WithRPM(rpm int) Config { c.RPM = rpm; return c }

// WithEntitlementTTL returns a copy with the entitlement cache TTL set.
func (c Config) WithEntitlementTTL(d time.Duration) Config { c.EntitlementTTL = d; return c }

// WithDegradedFailOpen returns a copy with the cold-cache fail-open posture set.
func (c Config) WithDegradedFailOpen(b bool) Config { c.DegradedFailOpen = b; return c }

// WithDegradedRPM returns a copy with the cold-cache degraded RPM cap set.
func (c Config) WithDegradedRPM(rpm int) Config { c.DegradedRPM = rpm; return c }

// WithIdentityCacheMax returns a copy with the last-known-good identity cache
// size cap set (0 = built-in default).
func (c Config) WithIdentityCacheMax(n int) Config { c.IdentityCacheMax = n; return c }

// WithUsageSpoolPath returns a copy with the durable usage-spool file path
// set (empty disables the spool).
func (c Config) WithUsageSpoolPath(path string) Config { c.UsageSpoolPath = path; return c }

// WithReconcileInterval returns a copy with the spool reconciler interval set
// (0 selects the built-in default).
func (c Config) WithReconcileInterval(d time.Duration) Config { c.ReconcileInterval = d; return c }

func (c Config) client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func (c Config) auth(req *http.Request) {
	if c.Secret != "" {
		req.Header.Set(HeaderRelayAuth, c.Secret)
	}
}

// ---------------------------------------------------------------------------
// Identity: POST {cp}/api/llm/resolve {"key":"<token>"} -> {account_id,tier}
// ---------------------------------------------------------------------------

// defaultIdentityTTL bounds how long a successfully-resolved principal is reused
// as last-known-good when cp is unreachable. Kept short so a revoked token stops
// being admitted quickly once cp recovers.
const defaultIdentityTTL = 30 * time.Second

// defaultIdentityCacheMax bounds how many last-known-good identity entries are
// retained. Without a cap the cache grows unbounded by distinct-token count over
// the process lifetime; with it, the cache is pruned lazily on insert (expired
// entries swept, then oldest evicted past the cap). Overridable via
// Config.IdentityCacheMax.
const defaultIdentityCacheMax = 4096

// Identity resolves a bearer token to a Vulos account via cp.
//
// It mirrors the BudgetGate's last-known-good behavior so a brief cp outage
// degrades gracefully instead of hard-401ing every request: a token that was
// SUCCESSFULLY resolved within the TTL is reused when cp is unreachable. It is
// fail-closed in every other respect — a token cp has never confirmed, or one cp
// explicitly rejects (4xx), is never admitted from cache, and a definitive
// rejection evicts any cached entry so a revoked token can't ride the TTL.
type Identity struct {
	cfg  Config
	http *http.Client
	ttl  time.Duration
	max  int // cap on retained last-known-good entries (<=0 disables the cap)

	mu    sync.Mutex
	cache map[string]idCacheEntry // token -> last successfully-resolved principal
}

type idCacheEntry struct {
	p  server.Principal
	at time.Time
}

// NewIdentity builds the cp Identity resolver.
func NewIdentity(cfg Config) *Identity {
	ttl := defaultIdentityTTL
	if cfg.EntitlementTTL > 0 {
		ttl = cfg.EntitlementTTL
	}
	max := defaultIdentityCacheMax
	if cfg.IdentityCacheMax > 0 {
		max = cfg.IdentityCacheMax
	}
	return &Identity{cfg: cfg, http: cfg.client(), ttl: ttl, max: max, cache: map[string]idCacheEntry{}}
}

type resolveRequest struct {
	Key string `json:"key"`
}

type resolveResponse struct {
	AccountID string `json:"account_id"`
	Tier      string `json:"tier"`
}

// Resolve implements server.Identity.
//
// Outcomes:
//   - cp 200 + account: resolved, cached as last-known-good, ok=true.
//   - cp explicit 4xx (e.g. 404 unknown token): ok=false AND any cached entry for
//     the token is evicted — a definitive rejection must not be overridden by the
//     last-known-good cache (revoked tokens stop working immediately).
//   - cp unreachable (transport error) or 5xx: DEGRADED. If the same token was
//     successfully resolved within the TTL, reuse that principal so a brief cp
//     blip doesn't 401 every in-flight session. Otherwise ok=false (a token cp
//     never confirmed is never admitted just because cp is down).
func (i *Identity) Resolve(ctx context.Context, token string) (server.Principal, bool) {
	raw, err := json.Marshal(resolveRequest{Key: token})
	if err != nil {
		return server.Principal{}, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.cfg.BaseURL+"/api/llm/resolve", bytes.NewReader(raw))
	if err != nil {
		return server.Principal{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	i.cfg.auth(req)

	resp, err := i.http.Do(req)
	if err != nil {
		// cp unreachable: degrade to last-known-good (fail-soft) if we have a
		// fresh, previously-confirmed principal for this token.
		return i.lastKnownGood(token)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var r resolveResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.AccountID == "" {
			return server.Principal{}, false
		}
		p := server.Principal{Token: token, AccountID: r.AccountID, Tier: r.Tier}
		i.mu.Lock()
		i.cache[token] = idCacheEntry{p: p, at: time.Now()}
		// Lazy sweep: drop expired entries and bound the cache size so it can't
		// grow unbounded by distinct-token count over the process lifetime.
		i.pruneCacheLocked()
		i.mu.Unlock()
		return p, true
	}

	// 5xx (or any other server-side failure): cp is degraded, not authoritative
	// — fall back to last-known-good rather than locking everyone out.
	if resp.StatusCode >= 500 {
		return i.lastKnownGood(token)
	}

	// Explicit client-side rejection (4xx, e.g. 404 unknown / revoked token):
	// definitive. Evict any cached entry so the token cannot ride the TTL.
	i.mu.Lock()
	delete(i.cache, token)
	i.mu.Unlock()
	return server.Principal{}, false
}

// pruneCacheLocked bounds the last-known-good identity cache. It first sweeps
// every entry whose age has passed the TTL (those can never be served again, so
// retaining them only leaks memory), then, if the cache still exceeds the size
// cap, evicts the oldest entries until it fits. Callers must hold i.mu.
//
// This preserves the existing semantics exactly: pruning only removes entries
// that are already unusable (expired) or, under memory pressure, the oldest
// ones; it never serves an entry that lastKnownGood/Resolve would have rejected.
func (i *Identity) pruneCacheLocked() {
	now := time.Now()
	for tok, ce := range i.cache {
		if now.Sub(ce.at) >= i.ttl {
			delete(i.cache, tok)
		}
	}
	if i.max <= 0 {
		return
	}
	// Over the cap even after the expiry sweep (lots of fresh distinct tokens):
	// evict the oldest entries until within the cap.
	for len(i.cache) > i.max {
		var oldestTok string
		var oldestAt time.Time
		first := true
		for tok, ce := range i.cache {
			if first || ce.at.Before(oldestAt) {
				oldestTok, oldestAt, first = tok, ce.at, false
			}
		}
		delete(i.cache, oldestTok)
	}
}

// lastKnownGood returns a previously-confirmed principal for token when one was
// resolved within the TTL, used only when cp is unreachable/degraded. It never
// admits a token that was never successfully resolved.
func (i *Identity) lastKnownGood(token string) (server.Principal, bool) {
	i.mu.Lock()
	ce, ok := i.cache[token]
	i.mu.Unlock()
	if ok && time.Since(ce.at) < i.ttl {
		log.Printf("cp: identity resolve unavailable — using last-known-good principal for account %s", ce.p.AccountID)
		return ce.p, true
	}
	return server.Principal{}, false
}

// ---------------------------------------------------------------------------
// BudgetGate: GET {cp}/api/entitlements?product=llm&account_id=<id>
//             -> {llm_enabled,llm_budget_usd,suspended}
// ---------------------------------------------------------------------------

// defaultEntitlementTTL bounds how long a fetched entitlement is reused as
// last-known-good when cp is unreachable.
const defaultEntitlementTTL = 30 * time.Second

// reservationHold is the per-request in-flight cost reserved against the budget
// while a request is outstanding. We don't know a request's real cost until it
// finishes, so we hold this nominal amount; it bounds concurrent over-commit to
// at most (in-flight requests x reservationHold) USD above the cp budget.
const reservationHold = 0.05

// defaultDegradedRPM is the conservative per-account requests-per-minute cap
// enforced in cold-cache degraded mode (cp unreachable, nothing cached) when the
// operator has not explicitly opted into fail-open. It bounds spend during a cp
// outage instead of allowing unbounded concurrency against real provider keys.
const defaultDegradedRPM = 20

// BudgetGate gates a request by the account's central LLM entitlements.
//
// Beyond the raw cp check it adds three safety layers that the audit flagged:
//
//   - RESERVATION: a per-account in-flight cost accumulator. Check adds a hold to
//     the account total and denies when budget-inflight<=0, so N concurrent
//     requests can't all pass on a near-zero balance. The hold is released when
//     the request completes (server defers BudgetDecision.Release).
//   - RPM: an in-process per-account request-rate cap for cp principals (which
//     carry no local key bucket), so they aren't unlimited.
//   - LAST-KNOWN-GOOD CACHE: a short-TTL entitlement cache. On a cp outage the
//     gate enforces the last known budget/suspension instead of failing fully
//     open. Cold cache + cp error -> allow but logged degraded.
type BudgetGate struct {
	cfg  Config
	http *http.Client
	ttl  time.Duration

	mu       sync.Mutex
	inflight map[string]float64       // account -> reserved in-flight USD
	rpm      map[string]*rpmWindow    // account -> per-minute request window
	degraded map[string]*rpmWindow    // account -> per-minute window for degraded mode
	cache    map[string]entCacheEntry // account -> last-known entitlement
}

// degradedRPM is the effective cold-cache degraded RPM cap (configured or default).
func (b *BudgetGate) degradedRPM() int {
	if b.cfg.DegradedRPM > 0 {
		return b.cfg.DegradedRPM
	}
	return defaultDegradedRPM
}

type rpmWindow struct {
	window int64 // unix-minute
	count  int
}

type entCacheEntry struct {
	ent entitlementResponse
	at  time.Time
}

// NewBudgetGate builds the cp BudgetGate.
func NewBudgetGate(cfg Config) *BudgetGate {
	ttl := defaultEntitlementTTL
	if cfg.EntitlementTTL > 0 {
		ttl = cfg.EntitlementTTL
	}
	return &BudgetGate{
		cfg:      cfg,
		http:     cfg.client(),
		ttl:      ttl,
		inflight: map[string]float64{},
		rpm:      map[string]*rpmWindow{},
		degraded: map[string]*rpmWindow{},
		cache:    map[string]entCacheEntry{},
	}
}

type entitlementResponse struct {
	LLMEnabled   bool    `json:"llm_enabled"`
	LLMBudgetUSD float64 `json:"llm_budget_usd"`
	Suspended    bool    `json:"suspended"`
}

// Check implements server.BudgetGate.
//
// FAIL-SOFT: a cp outage uses the last-known entitlement (within TTL) rather
// than unlimited access; only a cold cache + cp error falls fully open (logged).
// An EXPLICIT cp answer is enforced: denied when !llm_enabled || suspended ||
// remaining<=0, where remaining accounts for in-flight reservations.
func (b *BudgetGate) Check(ctx context.Context, p server.Principal) server.BudgetDecision {
	// Per-account request-rate cap (cp principals have no local key bucket).
	if b.cfg.RPM > 0 && !b.allowRPM(p.AccountID) {
		return server.BudgetDecision{RateLimited: true, Reason: "rate limit exceeded for account " + p.AccountID}
	}

	ent, ok := b.fetchEntitlement(ctx, p.AccountID)
	if !ok {
		// Cold cache and cp unreachable: we have no budget figure, so we cannot
		// place a real reservation. The DEFAULT posture bounds spend with a
		// conservative per-account RPM cap rather than failing fully open
		// (unbounded concurrency against real provider keys). Operators can opt
		// into the historical fail-open behavior via DegradedFailOpen.
		if b.cfg.DegradedFailOpen {
			log.Printf("cp: entitlement unavailable for %s (cp outage, cold cache) — failing OPEN (operator opt-in)", p.AccountID)
			return server.BudgetDecision{}
		}
		if !b.allowDegraded(p.AccountID) {
			log.Printf("cp: entitlement unavailable for %s (cp outage, cold cache) — degraded RPM cap hit, denying", p.AccountID)
			return server.BudgetDecision{RateLimited: true, Reason: "control plane unavailable; degraded rate limit for account " + p.AccountID}
		}
		log.Printf("cp: entitlement unavailable for %s (cp outage, cold cache) — allowing under degraded RPM cap (%d/min)", p.AccountID, b.degradedRPM())
		return server.BudgetDecision{}
	}

	switch {
	case ent.Suspended:
		return server.BudgetDecision{Denied: true, Reason: "account suspended"}
	case !ent.LLMEnabled:
		return server.BudgetDecision{Denied: true, Reason: "llm not enabled for account"}
	}

	// Reservation: deny if the budget net of in-flight holds is exhausted, else
	// place a hold and return a Release to free it on completion.
	b.mu.Lock()
	remaining := ent.LLMBudgetUSD - b.inflight[p.AccountID]
	if remaining <= 0 {
		b.mu.Unlock()
		return server.BudgetDecision{Denied: true, Reason: "llm budget exhausted"}
	}
	b.inflight[p.AccountID] += reservationHold
	b.mu.Unlock()

	acct := p.AccountID
	return server.BudgetDecision{Release: func() { b.release(acct) }}
}

// release frees one request's reservation hold for an account.
func (b *BudgetGate) release(account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.inflight[account] - reservationHold
	if v <= 0 {
		delete(b.inflight, account)
		return
	}
	b.inflight[account] = v
}

// allowRPM enforces a per-account fixed-window request cap. Returns false when
// the account has exceeded cfg.RPM in the current minute.
func (b *BudgetGate) allowRPM(account string) bool {
	now := time.Now().Unix() / 60
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.rpm[account]
	if w == nil || w.window != now {
		b.rpm[account] = &rpmWindow{window: now, count: 1}
		return true
	}
	if w.count >= b.cfg.RPM {
		return false
	}
	w.count++
	return true
}

// allowDegraded enforces the conservative cold-cache degraded per-account RPM
// cap. Returns false when the account has exceeded the degraded cap this minute.
// It uses a window map separate from the steady-state RPM so the two caps don't
// interfere.
func (b *BudgetGate) allowDegraded(account string) bool {
	limit := b.degradedRPM()
	now := time.Now().Unix() / 60
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.degraded[account]
	if w == nil || w.window != now {
		b.degraded[account] = &rpmWindow{window: now, count: 1}
		return true
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}

// fetchEntitlement gets the account entitlement from cp, caching it for TTL. On a
// cp transport/non-200/decode error it returns the last cached entitlement if one
// is present (any age — last-known-good beats unlimited during an outage). The
// second return is false only when cp failed AND nothing is cached.
func (b *BudgetGate) fetchEntitlement(ctx context.Context, account string) (entitlementResponse, bool) {
	// Fresh cache hit: skip the network entirely.
	b.mu.Lock()
	if ce, ok := b.cache[account]; ok && time.Since(ce.at) < b.ttl {
		b.mu.Unlock()
		return ce.ent, true
	}
	b.mu.Unlock()

	ent, err := b.queryCP(ctx, account)
	if err != nil {
		// cp unreachable/erroring: fall back to last-known entitlement if any.
		b.mu.Lock()
		ce, ok := b.cache[account]
		b.mu.Unlock()
		if ok {
			log.Printf("cp: entitlement fetch failed for %s (%v) — using last-known-good", account, err)
			return ce.ent, true
		}
		return entitlementResponse{}, false
	}
	b.mu.Lock()
	b.cache[account] = entCacheEntry{ent: ent, at: time.Now()}
	b.mu.Unlock()
	return ent, true
}

// queryCP performs the raw entitlement GET against cp.
func (b *BudgetGate) queryCP(ctx context.Context, account string) (entitlementResponse, error) {
	reqURL := fmt.Sprintf("%s/api/entitlements?product=%s&account_id=%s",
		b.cfg.BaseURL, product, url.QueryEscape(account))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return entitlementResponse{}, err
	}
	b.cfg.auth(req)

	resp, err := b.http.Do(req)
	if err != nil {
		return entitlementResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return entitlementResponse{}, fmt.Errorf("cp status %d", resp.StatusCode)
	}
	var ent entitlementResponse
	if err := json.NewDecoder(resp.Body).Decode(&ent); err != nil {
		return entitlementResponse{}, err
	}
	return ent, nil
}

// ---------------------------------------------------------------------------
// Usage: POST {cp}/api/usage
//   {"product":"llm","account_id":..,"kind":"llm_tokens","count":..,"cost_usd":..}
// ---------------------------------------------------------------------------

// usageQueueDepth bounds the in-memory retry queue. When full, the oldest
// pending record is dropped (logged) so a sustained cp outage can't grow memory
// unbounded — a deliberate, observable backpressure bound.
const usageQueueDepth = 1024

// usageMaxAttempts is the total number of POST attempts per record (1 initial +
// retries) on the FAST in-memory retry path before that path gives up on the
// record (see usageSpool for what happens next when the spool is enabled).
const usageMaxAttempts = 5

// defaultReconcileInterval is how often the background reconciler (started
// only when Config.UsageSpoolPath is set) retries every still-un-acked
// spooled usage record. It is intentionally much slower than the fast path's
// backoff (max 5s) since its job is to survive an OUTAGE, not a blip.
const defaultReconcileInterval = 30 * time.Second

// UsageLogger reports finalized per-request cost to cp. It is non-blocking to
// the response path and survives transient cp failures via a bounded
// in-memory retry queue with exponential backoff (the FAST path). Each record
// carries an idempotency key (see usageBody.IdempotencyKey / the
// Idempotency-Key header) so any retry — fast-path or reconciled — is deduped
// by cp and billed at most once.
//
// DURABILITY (closes TODO(billing-reconcile)): when Config.UsageSpoolPath is
// set, every record is durably written to an on-disk usageSpool BEFORE it is
// handed to the fast path, and removed from the spool only once cp
// acknowledges it (2xx). A background reconciler goroutine periodically
// re-POSTs everything still in the spool. Together these mean a record is
// dropped ONLY if: (a) the fast path never got to try it (queue full) AND (b)
// the process crashes before the spool write for it happened — an
// arbitrarily narrow window, vs. the previous "outlives 5 attempts or the
// process restarts" loss modes. Without a configured spool path, the logger
// keeps the historical in-memory-only behavior (fast path only; a record that
// exhausts usageMaxAttempts or a queue-full drop is lost from cp's
// perspective, though the separate optional JSONL ledger may still have it).
type UsageLogger struct {
	cfg  Config
	http *http.Client
	ch   chan usageItem
	sp   *usageSpool // nil unless Config.UsageSpoolPath is set

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup // tracks worker + reconcileLoop, so Close() can wait for both to fully exit
}

type usageItem struct {
	raw     []byte
	key     string // idempotency key (also sent as a header so cp can dedupe retries)
	attempt int
}

// NewUsageLogger builds the cp UsageLogger and starts its retry worker (and,
// when Config.UsageSpoolPath is set, its durable spool + reconciler).
func NewUsageLogger(cfg Config) *UsageLogger {
	u := &UsageLogger{cfg: cfg, http: cfg.client(), ch: make(chan usageItem, usageQueueDepth), stop: make(chan struct{})}
	if path := strings.TrimSpace(cfg.UsageSpoolPath); path != "" {
		sp, err := loadUsageSpool(path)
		if err != nil {
			log.Printf("cp: usage spool %s: load failed (%v) — continuing WITHOUT durable spool (in-memory-only billing delivery)", path, err)
		} else {
			u.sp = sp
			// Replay anything left over from a previous run (e.g. queued but
			// not yet acknowledged when the process crashed or restarted)
			// through the normal fast-path queue immediately; the reconciler
			// below is the ongoing backstop for whatever that doesn't clear.
			for key, raw := range sp.pending() {
				u.enqueue(usageItem{raw: raw, key: key, attempt: 0})
			}
			u.wg.Add(1)
			go func() { defer u.wg.Done(); u.reconcileLoop() }()
		}
	}
	u.wg.Add(1)
	go func() { defer u.wg.Done(); u.worker() }()
	return u
}

// reconcileLoop is the durability backstop: independently of the bounded fast
// path (which gives up after usageMaxAttempts), it periodically re-POSTs
// every record still present in the durable spool until cp acknowledges it.
// This is what makes an extended cp outage — or a record the fast path
// dropped for queue-full or gave-up-retrying — eventually reach cp once it
// recovers, instead of disappearing once the fast path stops trying.
func (u *UsageLogger) reconcileLoop() {
	interval := u.cfg.ReconcileInterval
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			u.reconcileOnce()
		case <-u.stop:
			return
		}
	}
}

// Close stops the logger's background goroutines (the fast-path worker and,
// when a spool is configured, the reconciler ticker) and BLOCKS until both
// have fully exited — including any reconcileOnce/post already in flight —
// so that once Close returns, nothing is still touching the spool file or
// making cp calls. It does not drain the in-memory queue or flush pending
// spool writes beyond what already happened; it exists so tests and
// graceful-shutdown callers don't leak goroutines (or race a subsequent
// removal of the spool's directory) past the logger's useful lifetime. Not
// required for correctness during normal operation — a live process simply
// leaves these running for its whole lifetime. Safe to call more than once.
func (u *UsageLogger) Close() {
	u.stopOnce.Do(func() { close(u.stop) })
	u.wg.Wait()
}

// reconcileOnce attempts one re-POST of every currently un-acked spooled
// record. A cp that is still down simply leaves those records in the spool
// for the next tick; cp already dedupes by Idempotency-Key so re-POSTing a
// record the fast path also has in flight is safe (billed at most once).
func (u *UsageLogger) reconcileOnce() {
	if u.sp == nil {
		return
	}
	for key, raw := range u.sp.pending() {
		if u.post(raw, key) {
			u.sp.ack(key)
		}
	}
}

type usageBody struct {
	// IdempotencyKey uniquely identifies this usage record so cp dedupes retries
	// (the same key is also sent in the Idempotency-Key header). Empty when the
	// source record carried no id.
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
	Product        string  `json:"product"`
	AccountID      string  `json:"account_id"`
	Kind           string  `json:"kind"`
	Count          int     `json:"count"`
	CostUSD        float64 `json:"cost_usd"`
}

// Log implements server.UsageLogger. It enqueues the finalized cost for delivery
// to cp keyed by the resolved account id. Non-blocking: if the queue is full it
// drops the record (logged) rather than stalling the response. Records with no
// account id are skipped (nothing to attribute to cp).
func (u *UsageLogger) Log(rec server.UsageRecord) {
	if rec.AccountID == "" {
		return
	}
	// BYOK requests are served with the account's OWN provider key — unmetered and
	// never billed centrally. They are still recorded locally (JSONL/dashboard) by
	// the core logger, but the cp billing sink skips them.
	if rec.BYOK {
		return
	}
	body := usageBody{
		IdempotencyKey: rec.ID,
		Product:        product,
		AccountID:      rec.AccountID,
		Kind:           "llm_tokens",
		Count:          rec.Total,
		CostUSD:        rec.CostUSD,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	// Durability: record this BEFORE handing it to the in-memory fast path, so
	// it survives a crash that happens immediately after this call returns.
	// No-op when no spool is configured (UsageSpoolPath unset).
	if u.sp != nil {
		u.sp.add(rec.ID, raw)
	}
	u.enqueue(usageItem{raw: raw, key: rec.ID, attempt: 0})
}

// enqueue pushes an item onto the FAST in-memory path, dropping the oldest
// queued item if the buffer is full so the producer (request path) never
// blocks. A drop here is no longer a durability loss when a spool is
// configured: Log() already wrote the record to the spool before calling
// enqueue, so the reconciler will still deliver it eventually.
func (u *UsageLogger) enqueue(it usageItem) {
	select {
	case u.ch <- it:
	default:
		// Queue full: make room by discarding the oldest, then enqueue.
		select {
		case dropped := <-u.ch:
			log.Printf("cp: usage queue full — dropping oldest record from the fast path (depth=%d)%s", usageQueueDepth, u.spoolNote())
			_ = dropped
		default:
		}
		select {
		case u.ch <- it:
		default:
			log.Printf("cp: usage queue full — dropping record from the fast path%s", u.spoolNote())
		}
	}
}

// spoolNote returns a short suffix clarifying whether a fast-path drop is
// still durably recoverable via the spool/reconciler, for log messages.
func (u *UsageLogger) spoolNote() string {
	if u.sp != nil {
		return " (durably spooled — the reconciler will retry it)"
	}
	return " (no usage spool configured — record is lost from cp's perspective)"
}

// worker drains the queue, POSTing each record and re-enqueuing failures with
// exponential backoff until usageMaxAttempts. It exits when Close() is called
// (or u.ch is closed, which nothing currently does in production use).
func (u *UsageLogger) worker() {
	for {
		var it usageItem
		select {
		case v, ok := <-u.ch:
			if !ok {
				return
			}
			it = v
		case <-u.stop:
			return
		}

		if u.post(it.raw, it.key) {
			if u.sp != nil {
				u.sp.ack(it.key)
			}
			continue
		}
		it.attempt++
		if it.attempt >= usageMaxAttempts {
			log.Printf("cp: usage POST gave up after %d attempts on the fast path%s", it.attempt, u.spoolNote())
			continue
		}
		// Back off, then re-enqueue. Done in a goroutine so the worker keeps
		// draining other records meanwhile.
		go func(it usageItem) {
			backoff := time.Duration(1<<uint(it.attempt-1)) * 100 * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			select {
			case <-time.After(backoff):
			case <-u.stop:
				return
			}
			u.enqueue(it)
		}(it)
	}
}

// post performs one usage POST; returns true on a 2xx (delivered). The
// idempotency key (when present) is sent as both the Idempotency-Key header and
// in the body so cp dedupes retries of the same record.
func (u *UsageLogger) post(raw []byte, key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.BaseURL+"/api/usage", bytes.NewReader(raw))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	u.cfg.auth(req)
	resp, err := u.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// MultiUsageLogger fans a record out to several loggers, so the cp sink composes
// with the existing JSONL logger (JSONL logging is never removed).
type MultiUsageLogger struct {
	loggers []server.UsageLogger
}

// NewMultiUsageLogger composes loggers (nil entries are dropped).
func NewMultiUsageLogger(loggers ...server.UsageLogger) *MultiUsageLogger {
	out := make([]server.UsageLogger, 0, len(loggers))
	for _, l := range loggers {
		if l != nil {
			out = append(out, l)
		}
	}
	return &MultiUsageLogger{loggers: out}
}

// Log implements server.UsageLogger by delegating to each composed logger.
func (m *MultiUsageLogger) Log(rec server.UsageRecord) {
	for _, l := range m.loggers {
		l.Log(rec)
	}
}

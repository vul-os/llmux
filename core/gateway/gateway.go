// Package gateway is llmux as a library: an in-process, OpenAI-compatible LLM
// gateway with routing, retries, failover, sovereignty enforcement, BYOK,
// caching, pricing and metering — and no HTTP surface of its own.
//
// core/server is one shell over this package (the HTTP gateway and admin
// console). It is not the only possible one, and nothing here imports it.
//
// The five rules this package holds to:
//
//  1. New starts no goroutines. Background work is opt-in via Run.
//  2. New reads no environment. config.FromEnv is an explicit call the caller
//     makes.
//  3. No package-level mutable state and no package logger — everything hangs
//     off the *Gateway, so two gateways in one process never interfere.
//  4. Readiness is explicit (Start/Run), never implied.
//  5. Pure core, opt-in I/O.
//
// The one qualification to rule 1: when the caller configures a Postgres DSN
// (cfg.Postgres), New builds the Postgres key store, which connects and
// migrates eagerly. That is an explicitly opted-into remote dependency; with no
// DSN — every default and library configuration — New opens nothing.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vul-os/llmux/core/cache"
	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/keys"
	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/pricing"
	"github.com/vul-os/llmux/core/provider"
	"github.com/vul-os/llmux/core/providers"
	"github.com/vul-os/llmux/core/router"
	"github.com/vul-os/llmux/core/sovereign"
)

// Gateway is an in-process llmux. Build it with New, dispatch with Chat /
// ChatStream / Embed, and (optionally) start its background work with Run.
type Gateway struct {
	cfg              *config.Config
	popts            provider.Options
	registry         *provider.Registry
	router           *router.Router
	sovereign        *sovereign.Policy
	keys             keys.Store
	identity         Identity
	budget           BudgetGate
	byok             BYOKStore
	externalIdentity bool
	cache            cache.Cache
	catalog          *pricing.Catalog
	pricingSources   []pricing.Source
	usage            UsageLogger
	stats            *UsageStats
	metrics          *Metrics
	log              *slog.Logger
	semantic         bool

	// rdb is the optional shared Redis client (rate limiting + cache across
	// replicas). It is constructed lazily-connected: go-redis dials on first
	// use, so building it opens no socket. Start pings it.
	rdb *redis.Client
}

// Option customizes a Gateway at construction. Options never start anything.
type Option func(*Gateway)

// WithLogger sets the structured logger. nil is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(g *Gateway) {
		if l != nil {
			g.log = l
		}
	}
}

// WithIdentity wires an external identity resolver (e.g. the cp adapter),
// activating the authenticated path even with no static keys configured.
func WithIdentity(id Identity) Option { return func(g *Gateway) { g.SetIdentity(id) } }

// WithBudgetGate wires an external budget gate (e.g. the cp adapter).
func WithBudgetGate(gate BudgetGate) Option { return func(g *Gateway) { g.SetBudgetGate(gate) } }

// WithBYOKStore wires a BYOK credential store.
func WithBYOKStore(store BYOKStore) Option { return func(g *Gateway) { g.SetBYOKStore(store) } }

// WithUsageLogger wires a usage sink (JSONL file, cp, ...).
func WithUsageLogger(l UsageLogger) Option { return func(g *Gateway) { g.SetUsageLogger(l) } }

// New builds a Gateway from config. It starts NO goroutines and — absent an
// explicitly configured Postgres DSN — opens NO sockets: no pricing sync, no
// spend flusher, no Redis ping. Call Run (or Start) to opt into that.
func New(cfg *config.Config, opts ...Option) (*Gateway, error) {
	if cfg == nil {
		return nil, errors.New("llmux: nil config")
	}
	g := &Gateway{
		cfg:      cfg,
		usage:    NopUsageLogger{},
		stats:    NewUsageStats(),
		metrics:  NewMetrics(),
		log:      NewLogger(cfg.LogLevel),
		catalog:  pricing.New(),
		semantic: false,
	}
	for _, o := range opts {
		o(g)
	}

	// Per-gateway adapter options. These used to be package-level variables in
	// core/provider that server.New MUTATED, so two gateways in one process
	// silently corrupted each other's response cap and drop_params.
	g.popts = provider.NewOptions()
	g.popts.MaxResponseBytes = cfg.MaxResponseBytes
	g.popts.DropParams = cfg.DropParams

	reg, err := providers.Build(cfg.Providers, g.popts, g.log)
	if err != nil {
		return nil, err
	}
	g.registry = reg

	// Warm start the price catalog from the on-disk cache, if configured.
	if cfg.Pricing.CatalogPath != "" {
		if err := g.catalog.Load(cfg.Pricing.CatalogPath); err == nil {
			g.log.Info("loaded price cache", "path", cfg.Pricing.CatalogPath, "models", g.catalog.Len())
		}
	}
	g.pricingSources = buildPricingSources(cfg)
	// Apply manual overrides synchronously so they take effect before first sync.
	applyOverrides(g.log, g.catalog, cfg)

	g.router = router.New(cfg.Routes, reg, g.catalog)

	// Optional shared Redis client (rate limiting + cache across replicas).
	// redis.NewClient does not dial; the pool connects on first use, and Start
	// pings it so a misconfigured address still fails loudly at startup.
	if cfg.Redis != "" {
		g.rdb = redis.NewClient(&redis.Options{Addr: cfg.Redis})
	}

	// Choose the key store: Postgres (cross-replica) > file (persistent) > memory.
	switch {
	case cfg.Postgres != "":
		var lim keys.Limiter
		if g.rdb != nil {
			lim = keys.NewRedisLimiter(g.rdb)
		}
		pg, err := keys.NewPGStore(context.Background(), cfg.Postgres, cfg.PostgresSchema, cfg.Keys, lim)
		if err != nil {
			return nil, err
		}
		g.keys = pg
		schema := cfg.PostgresSchema
		if schema == "" {
			schema = keys.DefaultSchema
		}
		g.log.Info("keys/spend in Postgres", "schema", schema)
	case cfg.KeyStorePath != "":
		fs, err := keys.NewFileStore(cfg.Keys, cfg.KeyStorePath)
		if err != nil {
			return nil, err
		}
		g.keys = fs
		g.log.Info("persisting key spend to disk", "path", cfg.KeyStorePath)
	default:
		g.keys = keys.NewMemStore(cfg.Keys)
	}

	// Standalone defaults for the auth seam. An in-process host is already
	// trusted; a cp adapter overrides these via SetIdentity/SetBudgetGate.
	if g.identity == nil {
		g.identity = StaticIdentity{Keys: g.keys}
	}
	if g.budget == nil {
		g.budget = NewStaticBudgetGate(g.keys)
	}

	// Sovereignty policy: classify every provider local vs egress and label the
	// posture at startup. This is the enforcement source of truth for "nothing
	// leaves the box unless you say so".
	g.sovereign = sovereign.NewPolicy(cfg.Providers)
	logSovereignty(g.log, g.sovereign)

	ttl := time.Duration(cfg.Cache.TTLSeconds) * time.Second
	switch {
	case cfg.Cache.Semantic:
		threshold := cfg.Cache.SimilarityThreshold
		if threshold <= 0 {
			threshold = 0.95
		}
		model := cfg.Cache.EmbeddingModel
		g.cache = cache.NewSemanticCache(gatewayEmbedder{g: g, model: model}, threshold, cfg.Cache.MaxEntries, ttl)
		g.semantic = true
		g.log.Info("semantic cache on", "model", model, "threshold", threshold)
	case cfg.Cache.Enabled && g.rdb != nil:
		g.cache = cache.NewRedisCache(g.rdb, ttl)
		g.log.Info("response cache in Redis")
	case cfg.Cache.Enabled:
		g.cache = cache.NewLRU(cfg.Cache.MaxEntries, ttl)
	}
	return g, nil
}

// Start opts into the gateway's background work and returns immediately. It
// pings Redis (so a misconfigured address fails at startup, not on the first
// request), starts the price-catalog syncer and starts the file key store's
// spend flusher. Everything it starts stops when ctx is cancelled.
//
// A library caller that never calls Start or Run gets ZERO background traffic:
// no goroutines, no outbound GETs to the price feeds.
func (g *Gateway) Start(ctx context.Context) error {
	if g.rdb != nil {
		if err := g.rdb.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis %s: %w", g.cfg.Redis, err)
		}
		g.log.Info("redis connected", "addr", g.cfg.Redis)
	}
	// Background price-catalog sync (best-effort; built-in/cached catalog meanwhile).
	if len(g.pricingSources) > 0 {
		syncer := pricing.NewSyncer(g.catalog, g.pricingSources,
			time.Duration(g.cfg.Pricing.SyncIntervalMinutes)*time.Minute, g.cfg.Pricing.CatalogPath)
		go syncer.Run(ctx)
	}
	// Persist key spend periodically when using the file-backed store.
	if fs, ok := g.keys.(*keys.FileStore); ok {
		go fs.StartFlusher(ctx, 5*time.Second)
	}
	return nil
}

// Run starts the background work (see Start) and blocks until ctx is done. It
// is entirely optional: Chat, ChatStream and Embed work without it.
func (g *Gateway) Run(ctx context.Context) error {
	if err := g.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// Close releases the gateway's resources: the Redis client and the Postgres
// pool. It does not stop background work — that is what cancelling Run's
// context does. Close is safe to call on a gateway that was never started.
func (g *Gateway) Close() error {
	var errs []error
	if g.rdb != nil {
		if err := g.rdb.Close(); err != nil {
			errs = append(errs, err)
		}
		g.rdb = nil
	}
	if c, ok := g.keys.(interface{ Close() }); ok {
		c.Close()
	}
	return errors.Join(errs...)
}

// Models returns the model ids the gateway can serve, as the price catalog
// knows them.
func (g *Gateway) Models() []string {
	list := g.catalog.Models()
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// ModelList returns the full OpenAI-shaped model listing (id, pricing, context
// window), which is what GET /v1/models serves.
func (g *Gateway) ModelList() openai.ModelList {
	return openai.ModelList{Object: "list", Data: g.catalog.Models()}
}

// Config returns the configuration the gateway was built from.
func (g *Gateway) Config() *config.Config { return g.cfg }

// Registry returns the provider registry.
func (g *Gateway) Registry() *provider.Registry { return g.registry }

// Router returns the model router.
func (g *Gateway) Router() *router.Router { return g.router }

// Keys returns the virtual-key store.
func (g *Gateway) Keys() keys.Store { return g.keys }

// Catalog returns the price catalog.
func (g *Gateway) Catalog() *pricing.Catalog { return g.catalog }

// Metrics returns the in-process counters.
func (g *Gateway) Metrics() *Metrics { return g.metrics }

// Logger returns the gateway's structured logger.
func (g *Gateway) Logger() *slog.Logger { return g.log }

// ProviderOptions returns the per-gateway adapter options (response cap,
// drop_params, HTTP clients).
func (g *Gateway) ProviderOptions() provider.Options { return g.popts }

// Cache reports whether a response cache is configured.
func (g *Gateway) Cache() cache.Cache { return g.cache }

// ---------------------------------------------------------------------------
// Pricing / metering helpers
// ---------------------------------------------------------------------------

// unmeterableBudgeted reports whether serving model on the primary route would be
// an UNMETERABLE charge against a budget-enforcing key: a non-BYOK request whose
// resolved model the catalog cannot price. attachCost/recordSpend would then
// record $0, the key's spend would never rise, OverBudget would never trip, and a
// budgeted key could burn unbounded real provider spend on the operator's central
// keys — the same fail-open class as an unchecked budget on a store error
// (hardened separately). Callers refuse PRE-FLIGHT so no upstream call happens.
//
// Unaffected (correctly return false): BYOK requests (unmetered by design — they
// spend the account's own key), and keys with no budget (BudgetUSD<=0: nothing to
// enforce, so a $0-logged unpriced request is acceptable and left as-is).
func (g *Gateway) unmeterableBudgeted(ctx context.Context, model, primaryProvider string) bool {
	if g.primaryBYOK(ctx, primaryProvider) {
		return false
	}
	k := keyFrom(ctx)
	if k == nil || k.BudgetUSD <= 0 {
		return false
	}
	return !g.catalog.HasPrice(model, primaryProvider)
}

// UnmeterableBudgeted is the exported form of unmeterableBudgeted, for dispatch
// paths outside this package (the HTTP shell's forwards).
func (g *Gateway) UnmeterableBudgeted(ctx context.Context, model, primaryProvider string) bool {
	return g.unmeterableBudgeted(ctx, model, primaryProvider)
}

// attachCost computes and attaches dollar cost to a usage record from the
// catalog (route-aware on the provider actually used), unless a provider
// already supplied it.
func (g *Gateway) attachCost(model, provider string, usage *openai.Usage) {
	if usage == nil || usage.Cost != nil {
		return
	}
	if c := g.catalog.Cost(model, provider, usage); c != nil {
		usage.Cost = c
	}
}

// AttachCost attaches dollar cost to a usage record. It is the exported form of
// attachCost, for dispatch paths outside this package.
func (g *Gateway) AttachCost(model, provider string, usage *openai.Usage) {
	g.attachCost(model, provider, usage)
}

// buildPricingSources assembles the ordered pricing sources from config.
func buildPricingSources(cfg *config.Config) []pricing.Source {
	var srcs []pricing.Source
	for _, u := range cfg.Pricing.Sources {
		srcs = append(srcs, pricing.SourceFromURL(u))
	}
	if cfg.Pricing.Azure {
		srcs = append(srcs, pricing.NewAzureSource())
	}
	if cfg.Pricing.OverridePath != "" {
		srcs = append(srcs, pricing.NewFileOverrideSource(cfg.Pricing.OverridePath))
	} else if len(cfg.Pricing.Overrides) > 0 {
		srcs = append(srcs, pricing.NewOverrideSource(convertOverrides(cfg.Pricing.Overrides)))
	}
	return srcs
}

// applyOverrides loads manual overrides into the catalog immediately.
func applyOverrides(log *slog.Logger, catalog *pricing.Catalog, cfg *config.Config) {
	if cfg.Pricing.OverridePath != "" {
		if p, err := pricing.NewFileOverrideSource(cfg.Pricing.OverridePath).Fetch(context.Background()); err == nil {
			catalog.SetSource(pricing.SourceNameOverride, pricing.PriorityOverride, p)
		} else {
			log.Warn("price override file could not be read", "path", cfg.Pricing.OverridePath, "err", err)
		}
	} else if len(cfg.Pricing.Overrides) > 0 {
		catalog.SetSource(pricing.SourceNameOverride, pricing.PriorityOverride, convertOverrides(cfg.Pricing.Overrides))
	}
}

func convertOverrides(m map[string]config.PriceOverride) map[string]pricing.Price {
	out := make(map[string]pricing.Price, len(m))
	for id, o := range m {
		out[id] = pricing.Price{
			Model: id, Provider: o.Provider,
			InputPerMTok: o.InputPerMTok, OutputPerMTok: o.OutputPerMTok,
			ContextWindow: o.ContextWindow, MaxOutput: o.MaxOutput, Capabilities: o.Capabilities,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// NewLogger builds a structured logger at the configured level. It is the
// gateway's only logging construct — nothing in this package writes to the
// package-level log.
func NewLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// GenID returns an OpenAI-style identifier with the given prefix.
func GenID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// AsProviderError extracts a *provider.Error from an error chain, or nil.
func AsProviderError(err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

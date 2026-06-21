// Package server is the HTTP gateway: it exposes the OpenAI-compatible API and
// dispatches to providers via the router. It speaks only canonical openai types.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/llmux/llmux/core/cache"
	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/keys"
	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/pricing"
	"github.com/llmux/llmux/core/provider"
	"github.com/llmux/llmux/core/providers"
	"github.com/llmux/llmux/core/router"
)

// Server is the llmux gateway.
type Server struct {
	cfg            *config.Config
	registry       *provider.Registry
	router         *router.Router
	keys           keys.Store
	cache          cache.Cache
	catalog        *pricing.Catalog
	pricingSources []pricing.Source
	usage          UsageLogger
	stats          *usageStats
	metrics        *Metrics
	log            *slog.Logger
	semantic       bool
	mux            *http.ServeMux
}

// New builds a Server from config.
func New(cfg *config.Config) (*Server, error) {
	reg, err := providers.Build(cfg.Providers)
	if err != nil {
		return nil, err
	}
	catalog := pricing.New()
	// Warm start from the on-disk cache, if configured.
	if cfg.Pricing.CatalogPath != "" {
		if err := catalog.Load(cfg.Pricing.CatalogPath); err == nil {
			log.Printf("llmux: loaded price cache from %s (%d models)", cfg.Pricing.CatalogPath, catalog.Len())
		}
	}
	sources := buildPricingSources(cfg)
	// Apply manual overrides synchronously so they take effect before first sync.
	applyOverrides(catalog, cfg)

	// Bound non-streaming upstream response bodies (0 = unlimited).
	provider.MaxResponseBytes = cfg.MaxResponseBytes
	// Strip configured params before forwarding to OpenAI-shaped upstreams.
	provider.DropParams = cfg.DropParams

	// Optional shared Redis client (rate limiting + cache across replicas).
	var rdb *redis.Client
	if cfg.Redis != "" {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.Redis})
		if err := rdb.Ping(context.Background()).Err(); err != nil {
			return nil, fmt.Errorf("redis %s: %w", cfg.Redis, err)
		}
		log.Printf("llmux: redis connected (%s)", cfg.Redis)
	}

	// Choose the key store: Postgres (cross-replica) > file (persistent) > memory.
	var keyStore keys.Store
	switch {
	case cfg.Postgres != "":
		var lim keys.Limiter
		if rdb != nil {
			lim = keys.NewRedisLimiter(rdb)
		}
		pg, err := keys.NewPGStore(context.Background(), cfg.Postgres, cfg.Keys, lim)
		if err != nil {
			return nil, err
		}
		keyStore = pg
		log.Printf("llmux: keys/spend in Postgres")
	case cfg.KeyStorePath != "":
		fs, err := keys.NewFileStore(cfg.Keys, cfg.KeyStorePath)
		if err != nil {
			return nil, err
		}
		keyStore = fs
		log.Printf("llmux: persisting key spend to %s", cfg.KeyStorePath)
	default:
		keyStore = keys.NewMemStore(cfg.Keys)
	}

	s := &Server{
		cfg:            cfg,
		registry:       reg,
		router:         router.New(cfg.Routes, reg, catalog),
		keys:           keyStore,
		catalog:        catalog,
		pricingSources: sources,
		usage:          NopUsageLogger{},
		stats:          newUsageStats(),
		metrics:        NewMetrics(),
		log:            newLogger(cfg.LogLevel),
		mux:            http.NewServeMux(),
	}
	ttl := time.Duration(cfg.Cache.TTLSeconds) * time.Second
	switch {
	case cfg.Cache.Semantic:
		threshold := cfg.Cache.SimilarityThreshold
		if threshold <= 0 {
			threshold = 0.95
		}
		model := cfg.Cache.EmbeddingModel
		s.cache = cache.NewSemanticCache(serverEmbedder{s: s, model: model}, threshold, cfg.Cache.MaxEntries, ttl)
		s.semantic = true
		log.Printf("llmux: semantic cache on (model=%q threshold=%.2f)", model, threshold)
	case cfg.Cache.Enabled && rdb != nil:
		s.cache = cache.NewRedisCache(rdb, ttl)
		log.Printf("llmux: response cache in Redis")
	case cfg.Cache.Enabled:
		s.cache = cache.NewLRU(cfg.Cache.MaxEntries, ttl)
	}
	s.routes()
	return s, nil
}

// attachCost computes and attaches dollar cost to a usage record from the
// catalog (route-aware on the provider actually used), unless a provider
// already supplied it.
func (s *Server) attachCost(model, provider string, usage *openai.Usage) {
	if usage == nil || usage.Cost != nil {
		return
	}
	if c := s.catalog.Cost(model, provider, usage); c != nil {
		usage.Cost = c
	}
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
func applyOverrides(catalog *pricing.Catalog, cfg *config.Config) {
	if cfg.Pricing.OverridePath != "" {
		if p, err := pricing.NewFileOverrideSource(cfg.Pricing.OverridePath).Fetch(context.Background()); err == nil {
			catalog.SetSource(pricing.SourceNameOverride, pricing.PriorityOverride, p)
		} else {
			log.Printf("llmux: override file: %v", err)
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

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	s.mux.HandleFunc("POST /v1/embeddings", s.handleEmbeddings)
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("GET /v1/catalog.json", s.handleCatalog)
	s.mux.HandleFunc("GET /admin/keys", s.handleAdminKeys)
	s.mux.HandleFunc("GET /admin/usage", s.handleAdminUsage)
	s.registerModalityRoutes()
	s.mountUI()
}

// Handler returns the root http.Handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.recoverMW(s.observeMW(s.metricsMW(s.authMW(s.mux))))
}

// Run starts listeners (TCP and/or unix socket) and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	// Background price-catalog sync (best-effort; built-in/cached catalog meanwhile).
	if len(s.pricingSources) > 0 {
		syncer := pricing.NewSyncer(s.catalog, s.pricingSources,
			time.Duration(s.cfg.Pricing.SyncIntervalMinutes)*time.Minute, s.cfg.Pricing.CatalogPath)
		go syncer.Run(ctx)
	}

	// Persist key spend periodically when using the file-backed store.
	if fs, ok := s.keys.(*keys.FileStore); ok {
		go fs.StartFlusher(ctx, 5*time.Second)
	}

	h := s.Handler()
	httpSrv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}

	var listeners []net.Listener
	if s.cfg.Server.Addr != "" {
		ln, err := net.Listen("tcp", s.cfg.Server.Addr)
		if err != nil {
			return err
		}
		log.Printf("llmux: listening on http://%s", ln.Addr())
		listeners = append(listeners, ln)
	}
	if s.cfg.Server.SocketPath != "" {
		// Fresh unix socket: remove any stale file first.
		_ = os.Remove(s.cfg.Server.SocketPath)
		ln, err := net.Listen("unix", s.cfg.Server.SocketPath)
		if err != nil {
			return err
		}
		_ = os.Chmod(s.cfg.Server.SocketPath, 0o600) // owner-only
		log.Printf("llmux: listening on unix://%s", s.cfg.Server.SocketPath)
		listeners = append(listeners, ln)
	}
	if len(listeners) == 0 {
		return errors.New("no listeners configured")
	}

	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(l net.Listener) {
			if err := httpSrv.Serve(l); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}(ln)
	}

	select {
	case <-ctx.Done():
		log.Printf("llmux: shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("llmux: panic: %v", rec)
				writeError(w, http.StatusInternalServerError,
					openai.NewError("internal error", "internal_error", ""))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authMW authenticates requests. If virtual keys are configured, it validates
// the bearer token, enforces the per-key rate limit and budget, and attaches the
// key to the request context. Otherwise it falls back to the master key, or to
// open access (local sidecar mode) when neither is set.
func (s *Server) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public: health and the embedded web app (the dashboard authenticates
		// to /admin client-side with the master key).
		if r.URL.Path == "/health" || r.URL.Path == "/ui" || strings.HasPrefix(r.URL.Path, "/ui/") {
			next.ServeHTTP(w, r)
			return
		}
		token := bearer(r)

		// Admin endpoints require the master key (never a virtual key). When no
		// master key is set (local mode), they're open like everything else.
		if strings.HasPrefix(r.URL.Path, "/admin") {
			if s.cfg.Server.MasterKey != "" && !s.masterKeyValid(token) {
				writeError(w, http.StatusUnauthorized,
					openai.NewError("admin endpoints require the master key", "invalid_request_error", "invalid_api_key"))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if len(s.cfg.Keys) > 0 {
			k, ok := s.keys.Lookup(token)
			if !ok {
				writeError(w, http.StatusUnauthorized,
					openai.NewError("invalid api key", "invalid_request_error", "invalid_api_key"))
				return
			}
			if s.keys.OverBudget(token) {
				writeError(w, http.StatusPaymentRequired,
					openai.NewError("budget exceeded for key "+k.Name, "insufficient_quota", "budget_exceeded"))
				return
			}
			if !s.keys.Allow(token) {
				writeError(w, http.StatusTooManyRequests,
					openai.NewError("rate limit exceeded for key "+k.Name, "rate_limit_error", "rate_limit_exceeded"))
				return
			}
			r = r.WithContext(withKey(r.Context(), k))
			next.ServeHTTP(w, r)
			return
		}

		if s.cfg.Server.MasterKey != "" {
			if !s.masterKeyValid(token) {
				writeError(w, http.StatusUnauthorized,
					openai.NewError("invalid api key", "invalid_request_error", "invalid_api_key"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	return strings.TrimPrefix(h, "Bearer ")
}

// masterKeyValid compares a token to the master key in constant time.
func (s *Server) masterKeyValid(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Server.MasterKey)) == 1
}

// isAdmin reports whether the request is authorized for privileged disclosure.
// With no master key configured (local mode), everything is treated as admin.
func (s *Server) isAdmin(r *http.Request) bool {
	if s.cfg.Server.MasterKey == "" {
		return true
	}
	return s.masterKeyValid(bearer(r))
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Unauthenticated callers get a minimal response; the provider/topology
	// list is disclosed only to the master key.
	if !s.isAdmin(r) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	provs := make([]map[string]string, 0, len(s.cfg.Providers))
	for _, p := range s.cfg.Providers {
		provs = append(provs, map[string]string{
			"name": p.Name, "type": string(p.Type), "stability": providers.Stability(p.Type),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"providers": provs,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := openai.ModelList{Object: "list", Data: s.catalog.Models()}
	writeJSON(w, http.StatusOK, list)
}

// handleCatalog exports the merged price catalog as open JSON — the community
// can consume this directly, fresher than a PR-gated file.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": s.catalog.Updated().UTC().Format(time.RFC3339),
		"count":   s.catalog.Len(),
		"prices":  s.catalog.Snapshot(),
	})
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e *openai.ErrorResponse) {
	writeJSON(w, status, e)
}

// writeRawJSON writes a pre-serialized JSON body with a 200 status (used for
// cache hits and the once-marshaled chat response — avoids a re-marshal).
func writeRawJSON(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// asProviderError extracts a *provider.Error from an error chain, or nil.
func asProviderError(err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// writeProviderError relays a provider failure with its upstream status/body.
// Unexpected (non-provider) errors are logged server-side and returned generic,
// so internal details (e.g. outbound URLs) are never echoed to clients.
func writeProviderError(w http.ResponseWriter, err error) {
	if pe := asProviderError(err); pe != nil {
		writeJSON(w, pe.Status(), pe.Body)
		return
	}
	log.Printf("llmux: upstream error: %v", err)
	writeError(w, http.StatusBadGateway,
		openai.NewError("upstream request failed", "upstream_error", ""))
}

// genID returns an OpenAI-style identifier with the given prefix.
func genID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

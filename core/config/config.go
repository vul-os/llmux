// Package config loads llmux configuration from a JSON file and environment
// variables. It is dependency-free (stdlib only) so the core always builds.
//
// Resolution order (later wins): built-in defaults -> config file -> env vars.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the root configuration.
type Config struct {
	Server    ServerConfig     `json:"server"`
	Providers []ProviderConfig `json:"providers"`
	Routes    []RouteConfig    `json:"routes"`
	Pricing   PricingConfig    `json:"pricing"`
	Retry     RetryConfig      `json:"retry"`
	Cache     CacheConfig      `json:"cache"`
	Keys      []KeyConfig      `json:"keys"`
	// KeyStorePath, if set, persists per-key spend to a JSON file (budgets
	// survive restarts). Ignored when Postgres is set.
	KeyStorePath string `json:"key_store_path"`

	// Postgres DSN. When set, keys/spend/budgets live in Postgres (correct
	// across replicas) instead of in-memory/file.
	//
	// The DSN is resolved (later wins) from: this field -> env LLMUX_POSTGRES
	// (legacy, product-specific fallback) -> env DATABASE_URL -> env
	// VULOS_DATABASE_URL (the shared Neon DSN; preferred for cloud
	// consolidation). When the shared DSN is used, all llmux tables live under a
	// dedicated schema (PostgresSchema, default "llmux") so llmux can share one
	// database with the other Vulos products without name collisions.
	Postgres string `json:"postgres"`
	// PostgresSchema is the Postgres schema that holds llmux's tables. It lets
	// llmux share one database (e.g. a single Neon database) with other products.
	// Empty defaults to "llmux" whenever Postgres is set. Resolved from env
	// LLMUX_POSTGRES_SCHEMA.
	PostgresSchema string `json:"postgres_schema"`
	// Redis address (host:port). When set, rate limiting and (if caching is
	// enabled) the response cache use Redis — correct across replicas.
	Redis string `json:"redis"`

	// UpstreamTimeoutSeconds bounds a single non-streaming upstream call
	// (0 = no extra deadline beyond the client default). Streaming relies on
	// client-disconnect cancellation instead.
	UpstreamTimeoutSeconds int `json:"upstream_timeout_seconds"`
	// MaxResponseBytes bounds non-streaming upstream response bodies (0 = unlimited).
	MaxResponseBytes int64 `json:"max_response_bytes"`
	// DropParams lists request body fields to strip before forwarding to
	// OpenAI-shaped (passthrough/Azure) upstreams — e.g. params a given fleet
	// rejects. Avoids surfacing upstream 400s.
	DropParams []string `json:"drop_params"`

	LogLevel string `json:"log_level"`

	// UsageLogPath, if set, appends every usage record as one JSON line
	// (JSONL) to this file — a durable local ledger independent of any
	// control-plane billing seam. Resolved (later wins) from this field or env
	// LLMUX_USAGE_LOG; previously this was env-only with no config-file
	// counterpart.
	UsageLogPath string `json:"usage_log_path"`

	// CP optionally points llmux at a Vulos control plane ("cp" / vulos-cloud)
	// for central identity/budget/usage. Empty = standalone (the default): the
	// gateway uses its static keys and never talks to cp. This config is read by
	// the composition root (cmd/llmux) to wire the OPTIONAL integration/cp
	// adapter; the core gateway never imports it.
	CP CPConfig `json:"cp"`

	// BYOK configures per-account "bring your own key" storage. When a KEK is
	// present, accounts can register their own provider keys (encrypted at rest)
	// and requests for those providers use the account's key, unmetered. Empty =
	// BYOK disabled: every request uses the central provider keys.
	BYOK BYOKConfig `json:"byok"`
}

// BYOKConfig configures encrypted per-account BYOK key storage.
type BYOKConfig struct {
	// KEK is the 32-byte key-encryption key (raw, 64-char hex, or base64) used to
	// seal BYOK keys at rest. Prefer setting it via the LLMUX_BYOK_KEK env var
	// rather than in the config file. Empty = BYOK disabled.
	KEK string `json:"kek"`
	// KEKEnv names an env var to read the KEK from (alternative to KEK).
	KEKEnv string `json:"kek_env"`
	// StorePath persists the encrypted BYOK store to disk. Empty = in-memory only
	// (keys are lost on restart).
	StorePath string `json:"store_path"`
}

// ResolveKEK returns the effective KEK string, reading KEKEnv if KEK is empty.
func (b BYOKConfig) ResolveKEK() string {
	if b.KEK != "" {
		return b.KEK
	}
	if b.KEKEnv != "" {
		return os.Getenv(b.KEKEnv)
	}
	return ""
}

// CPConfig configures the optional control-plane integration.
type CPConfig struct {
	// URL is the cp base URL (e.g. https://cp.vulos.to). Empty = standalone.
	URL string `json:"cp_url"`
	// SharedSecret authenticates outbound cp calls via the X-Relay-Auth header.
	SharedSecret string `json:"cp_shared_secret"`
	// RPM is the per-account requests-per-minute cap applied to cp-resolved
	// principals (which carry no local key bucket). 0 = no cp-side RPM limit.
	RPM int `json:"cp_rpm"`
	// EntitlementTTLSeconds bounds how long a fetched entitlement is cached and
	// reused if cp becomes unreachable (last-known-good). 0 = a 30s default.
	EntitlementTTLSeconds int `json:"cp_entitlement_ttl_seconds"`
	// DegradedFailOpen, when true, makes the budget gate fail fully OPEN when cp
	// is unreachable AND nothing is cached for the account (cold cache). This was
	// the historical behavior but it allows unbounded concurrency against real
	// provider keys during a cp outage. It is OFF by default: the default
	// degraded posture is bounded (DegradedRPM). Self-hosters who accept the
	// spend risk can opt back into fail-open.
	DegradedFailOpen bool `json:"cp_degraded_fail_open"`
	// DegradedRPM is the conservative per-account requests-per-minute cap applied
	// ONLY in cold-cache degraded mode (cp unreachable, no cached entitlement),
	// when DegradedFailOpen is false. 0 selects a built-in conservative default.
	// This bounds spend during a cp outage instead of failing fully open.
	DegradedRPM int `json:"cp_degraded_rpm"`
	// UsageSpoolPath, if set, durably persists cp's pending (not-yet-acked)
	// usage records to this file so they survive a process restart or crash
	// instead of relying solely on the bounded in-memory retry queue
	// (integration/cp.UsageLogger). A background reconciler retries every
	// un-acked record until cp acknowledges it (idempotent via
	// Idempotency-Key), so an extended cp outage no longer silently drops
	// billing records. Empty = no spool (in-memory-only, the historical
	// behavior). Resolved from env LLMUX_CP_USAGE_SPOOL_PATH.
	UsageSpoolPath string `json:"cp_usage_spool_path"`
}

// RetryConfig controls automatic retries and provider fallback.
type RetryConfig struct {
	// MaxRetries is the number of retries per target on retryable errors.
	MaxRetries int `json:"max_retries"`
	// BackoffMS is the base backoff between retries (exponential).
	BackoffMS int `json:"backoff_ms"`
}

// CacheConfig controls response caching.
type CacheConfig struct {
	// Enabled turns on exact-match response caching for non-streaming requests.
	Enabled bool `json:"enabled"`
	// TTLSeconds is how long entries live (0 = no expiry).
	TTLSeconds int `json:"ttl_seconds"`
	// MaxEntries bounds the in-memory cache (0 = default 10000).
	MaxEntries int `json:"max_entries"`

	// Semantic switches to a semantic (embedding-similarity) cache instead of
	// exact-match. Requires EmbeddingModel to be routable.
	Semantic bool `json:"semantic"`
	// EmbeddingModel is the model used to embed prompts for semantic matching.
	EmbeddingModel string `json:"embedding_model"`
	// SimilarityThreshold is the minimum cosine similarity for a hit (0 = 0.95).
	SimilarityThreshold float64 `json:"similarity_threshold"`
}

// KeyConfig is a statically-configured virtual key with limits.
type KeyConfig struct {
	// Key is the bearer token clients present.
	Key string `json:"key"`
	// Name is a human label for logs/usage.
	Name string `json:"name"`
	// BudgetUSD caps cumulative spend (0 = unlimited).
	BudgetUSD float64 `json:"budget_usd"`
	// RPM caps requests per minute (0 = unlimited).
	RPM int `json:"rpm"`
	// AllowedModels, if non-empty, restricts which models this key may use.
	AllowedModels []string `json:"allowed_models,omitempty"`
}

// ServerConfig controls how the gateway listens.
type ServerConfig struct {
	// Addr is the TCP listen address (e.g. ":4000"). Empty disables TCP.
	Addr string `json:"addr"`
	// SocketPath, when set, makes the server listen on a unix socket. This is
	// how local sidecar mode talks to per-language packages.
	SocketPath string `json:"socket_path"`
	// MasterKey, if set, is required as a bearer token on every request unless
	// virtual keys are configured.
	MasterKey string `json:"master_key"`

	// InsecureKeyless opts INTO running keyless (no master key, no virtual keys)
	// while bound to a NON-loopback TCP address — i.e. an OPEN proxy reachable by
	// anyone who can connect, with /admin and /metrics unauthenticated. This is a
	// deliberate footgun override: by default a keyless server refuses to bind a
	// non-loopback address (fail closed) and a keyless loopback bind stays
	// unauthenticated for dev ergonomics. Resolved from env LLMUX_INSECURE_KEYLESS
	// (1/true). Leave false unless you fully understand the exposure.
	InsecureKeyless bool `json:"insecure_keyless"`
}

// ProviderType enumerates how a provider is reached.
type ProviderType string

const (
	// TypePassthrough forwards OpenAI-shaped requests with only key/base_url
	// swapped (OpenAI, DeepSeek, Groq, Together, xAI, OpenRouter, Ollama, ...).
	TypePassthrough ProviderType = "passthrough"
	// TypeAnthropic uses the Anthropic adapter.
	TypeAnthropic ProviderType = "anthropic"
	// TypeGemini uses the Google Gemini adapter.
	TypeGemini ProviderType = "gemini"
	// TypeCohere uses the Cohere v2 adapter.
	TypeCohere ProviderType = "cohere"
	// TypeBedrock uses the AWS Bedrock (Anthropic Claude) adapter.
	TypeBedrock ProviderType = "bedrock"
	// TypeAzure uses the Azure OpenAI adapter (api-key header, deployment URLs).
	TypeAzure ProviderType = "azure"
)

// ProviderConfig configures one upstream provider.
type ProviderConfig struct {
	Name    string       `json:"name"`
	Type    ProviderType `json:"type"`
	BaseURL string       `json:"base_url"`
	// APIKey may be set directly, or APIKeyEnv may name an env var to read.
	APIKey    string            `json:"api_key"`
	APIKeyEnv string            `json:"api_key_env"`
	Headers   map[string]string `json:"headers,omitempty"`

	// AllowEgress is the operator's explicit opt-in to let this provider send
	// data OFF the box. It is only meaningful for providers whose base URL is a
	// non-local (remote) endpoint; local (loopback / unix-socket) providers are
	// always allowed. This is llmux's sovereignty gate: without it, a non-local
	// provider is DENIED at dispatch and nothing leaves the box. See core/sovereign.
	AllowEgress bool `json:"allow_egress,omitempty"`

	// Tier is the operator's explicit sovereignty tier declaration for this
	// provider — "where your AI runs". One of "local", "sovereign", "brokered",
	// "external", or "" (auto = derive from locality: loopback→local, else
	// external). A loopback endpoint is ALWAYS classified local regardless of
	// this field; sovereign/brokered are trust declarations that only apply to
	// off-box endpoints. See core/sovereign for classification + enforcement.
	Tier string `json:"tier,omitempty"`

	// AllowBrokered is the operator's opt-in to permit calls to a provider
	// classified in the "brokered" tier (a named third party under a no-train
	// agreement). AllowEgress also permits brokered calls (it is the broader
	// escape hatch); AllowBrokered permits brokered WITHOUT unlocking raw
	// external egress. Ignored for local/sovereign (always allowed) and for
	// external (requires AllowEgress).
	AllowBrokered bool `json:"allow_brokered,omitempty"`
}

// ResolveKey returns the effective API key, reading APIKeyEnv if APIKey is empty.
func (p ProviderConfig) ResolveKey() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

// RouteConfig maps a client-visible model name (possibly an alias) to a
// provider + upstream model name. The first matching route wins; "*" matches any.
type RouteConfig struct {
	// Model is the client-facing model name or alias. "*" is a catch-all.
	Model string `json:"model"`
	// Provider is the name of a configured provider.
	Provider string `json:"provider"`
	// TargetModel overrides the upstream model name (defaults to Model).
	TargetModel string `json:"target_model,omitempty"`
	// Fallbacks lists provider names to try if the primary fails.
	Fallbacks []string `json:"fallbacks,omitempty"`

	// Strategy selects among Candidates: "" (use Provider), "least-cost".
	Strategy string `json:"strategy,omitempty"`
	// Candidates are the pool a strategy chooses from.
	Candidates []Candidate `json:"candidates,omitempty"`
}

// Candidate is one (provider, model) option for a routing strategy.
type Candidate struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// PricingConfig controls the price catalog.
type PricingConfig struct {
	// CatalogPath is where the merged catalog is cached on disk (warm start).
	CatalogPath string `json:"catalog_path"`
	// SyncIntervalMinutes controls how often the catalog refreshes (0 = off).
	SyncIntervalMinutes int `json:"sync_interval_minutes"`
	// Sources lists URLs to sync from (OpenRouter, LiteLLM JSON, ...).
	Sources []string `json:"sources"`
	// OverridePath is a JSON file of model->price overrides (highest precedence).
	OverridePath string `json:"override_path"`
	// Overrides are inline model->price overrides (highest precedence).
	Overrides map[string]PriceOverride `json:"overrides"`
	// Azure enables the Azure Retail Prices source for Azure OpenAI models.
	Azure bool `json:"azure_pricing"`
}

// PriceOverride is a manually-pinned price (authoritative; wins over all feeds).
// Costs are USD per 1,000,000 tokens.
type PriceOverride struct {
	Provider      string   `json:"provider,omitempty"`
	InputPerMTok  float64  `json:"input_per_mtok"`
	OutputPerMTok float64  `json:"output_per_mtok"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxOutput     int      `json:"max_output,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

// Default returns a config with sane defaults and providers auto-detected from
// well-known environment variables, so llmux works out of the box.
func Default() *Config {
	c := &Config{
		Server:   ServerConfig{Addr: ":4000"},
		LogLevel: "info",
		Retry:    RetryConfig{MaxRetries: 2, BackoffMS: 200},
		Cache:    CacheConfig{MaxEntries: 10000},
		Pricing: PricingConfig{
			CatalogPath:         "",
			SyncIntervalMinutes: 360,
			Sources: []string{
				"https://openrouter.ai/api/v1/models",
				"https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json",
			},
		},
	}
	c.autoDetectProviders()
	return c
}

// knownProvider describes a provider we can auto-wire from an env var.
type knownProvider struct {
	name    string
	typ     ProviderType
	baseURL string
	env     string
}

var knownProviders = []knownProvider{
	{"openai", TypePassthrough, "https://api.openai.com/v1", "OPENAI_API_KEY"},
	{"anthropic", TypeAnthropic, "https://api.anthropic.com/v1", "ANTHROPIC_API_KEY"},
	{"gemini", TypeGemini, "https://generativelanguage.googleapis.com/v1beta", "GEMINI_API_KEY"},
	{"deepseek", TypePassthrough, "https://api.deepseek.com/v1", "DEEPSEEK_API_KEY"},
	{"groq", TypePassthrough, "https://api.groq.com/openai/v1", "GROQ_API_KEY"},
	{"mistral", TypePassthrough, "https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
	{"together", TypePassthrough, "https://api.together.xyz/v1", "TOGETHER_API_KEY"},
	{"fireworks", TypePassthrough, "https://api.fireworks.ai/inference/v1", "FIREWORKS_API_KEY"},
	{"xai", TypePassthrough, "https://api.x.ai/v1", "XAI_API_KEY"},
	{"openrouter", TypePassthrough, "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
	{"cohere", TypeCohere, "https://api.cohere.com/v2", "COHERE_API_KEY"},
}

func (c *Config) autoDetectProviders() {
	// Sovereign default: an on-box local model backend. Detected first so it is
	// the natural primary. Ollama and llama.cpp both expose an OpenAI-compatible
	// endpoint, so a passthrough provider reaches them with zero adapter code.
	if base := localBackendURL(); base != "" {
		c.Providers = append(c.Providers, ProviderConfig{
			Name: LocalProviderName, Type: TypePassthrough, BaseURL: base,
			// Local servers usually need no key; allow an optional one.
			APIKeyEnv: "LLMUX_LOCAL_API_KEY",
		})
	}
	for _, kp := range knownProviders {
		if os.Getenv(kp.env) == "" {
			continue
		}
		c.Providers = append(c.Providers, ProviderConfig{
			Name: kp.name, Type: kp.typ, BaseURL: kp.baseURL, APIKeyEnv: kp.env,
		})
	}
}

// LocalProviderName is the conventional name of the auto-detected on-box
// sovereign backend.
const LocalProviderName = "local"

// localBackendURL resolves the on-box model server's OpenAI-compatible base URL
// from the environment, or "" if none is configured. Resolution (later wins):
//   - OLLAMA_HOST (host[:port] or URL) -> "<host>/v1"
//   - LLMUX_LOCAL_BASE_URL (explicit, full base URL incl. /v1)
func localBackendURL() string {
	base := ""
	if v := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); v != "" {
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		base = strings.TrimRight(v, "/") + "/v1"
	}
	if v := strings.TrimSpace(os.Getenv("LLMUX_LOCAL_BASE_URL")); v != "" {
		base = v
	}
	return base
}

// Load builds the configuration from defaults, an optional JSON file at path
// (ignored if path is empty or missing), then environment overrides.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			// A file may omit providers; only overwrite slices it provides.
			fileCfg := &Config{}
			if err := json.Unmarshal(data, fileCfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
			c.merge(fileCfg)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	c.applyEnv()
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// applyDefaults fills in convenience defaults after config + env are resolved.
// Sovereign default routing: if a local on-box provider is configured but no
// routes are, add a catch-all route to it. This makes "runs on YOUR instance"
// the zero-config default — any model name resolves to the local backend, and
// no request can silently reach a remote endpoint.
func (c *Config) applyDefaults() {
	if len(c.Routes) > 0 {
		return
	}
	if _, ok := c.ProviderByName(LocalProviderName); ok {
		c.Routes = append(c.Routes, RouteConfig{Model: "*", Provider: LocalProviderName})
	}
}

func (c *Config) merge(o *Config) {
	if o.Server.Addr != "" {
		c.Server.Addr = o.Server.Addr
	}
	if o.Server.SocketPath != "" {
		c.Server.SocketPath = o.Server.SocketPath
	}
	if o.Server.MasterKey != "" {
		c.Server.MasterKey = o.Server.MasterKey
	}
	if o.Server.InsecureKeyless {
		c.Server.InsecureKeyless = true
	}
	if len(o.Providers) > 0 {
		c.Providers = o.Providers
	}
	if len(o.Routes) > 0 {
		c.Routes = o.Routes
	}
	if o.Pricing.CatalogPath != "" {
		c.Pricing.CatalogPath = o.Pricing.CatalogPath
	}
	if o.Pricing.SyncIntervalMinutes != 0 {
		c.Pricing.SyncIntervalMinutes = o.Pricing.SyncIntervalMinutes
	}
	if len(o.Pricing.Sources) > 0 {
		c.Pricing.Sources = o.Pricing.Sources
	}
	if o.LogLevel != "" {
		c.LogLevel = o.LogLevel
	}
	if o.UsageLogPath != "" {
		c.UsageLogPath = o.UsageLogPath
	}
	if o.Retry.MaxRetries != 0 {
		c.Retry.MaxRetries = o.Retry.MaxRetries
	}
	if o.Retry.BackoffMS != 0 {
		c.Retry.BackoffMS = o.Retry.BackoffMS
	}
	if o.Cache.Enabled || o.Cache.Semantic {
		c.Cache = o.Cache
	}
	if len(o.Keys) > 0 {
		c.Keys = o.Keys
	}
	if o.KeyStorePath != "" {
		c.KeyStorePath = o.KeyStorePath
	}
	if o.Postgres != "" {
		c.Postgres = o.Postgres
	}
	if o.PostgresSchema != "" {
		c.PostgresSchema = o.PostgresSchema
	}
	if o.Redis != "" {
		c.Redis = o.Redis
	}
	if o.UpstreamTimeoutSeconds != 0 {
		c.UpstreamTimeoutSeconds = o.UpstreamTimeoutSeconds
	}
	if o.MaxResponseBytes != 0 {
		c.MaxResponseBytes = o.MaxResponseBytes
	}
	if len(o.DropParams) > 0 {
		c.DropParams = o.DropParams
	}
	c.mergeCP(o)
	c.mergeBYOK(o)
}

// mergeCP applies a file's control-plane block (later wins on set fields).
func (c *Config) mergeCP(o *Config) {
	if o.CP.URL != "" {
		c.CP.URL = o.CP.URL
	}
	if o.CP.SharedSecret != "" {
		c.CP.SharedSecret = o.CP.SharedSecret
	}
	if o.CP.RPM != 0 {
		c.CP.RPM = o.CP.RPM
	}
	if o.CP.EntitlementTTLSeconds != 0 {
		c.CP.EntitlementTTLSeconds = o.CP.EntitlementTTLSeconds
	}
	if o.CP.DegradedFailOpen {
		c.CP.DegradedFailOpen = true
	}
	if o.CP.DegradedRPM != 0 {
		c.CP.DegradedRPM = o.CP.DegradedRPM
	}
	if o.CP.UsageSpoolPath != "" {
		c.CP.UsageSpoolPath = o.CP.UsageSpoolPath
	}
}

// mergeBYOK applies a file's BYOK block (later wins on set fields).
func (c *Config) mergeBYOK(o *Config) {
	if o.BYOK.KEK != "" {
		c.BYOK.KEK = o.BYOK.KEK
	}
	if o.BYOK.KEKEnv != "" {
		c.BYOK.KEKEnv = o.BYOK.KEKEnv
	}
	if o.BYOK.StorePath != "" {
		c.BYOK.StorePath = o.BYOK.StorePath
	}
}

func (c *Config) applyEnv() {
	if v := os.Getenv("LLMUX_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("LLMUX_SOCKET"); v != "" {
		c.Server.SocketPath = v
	}
	if v := os.Getenv("LLMUX_MASTER_KEY"); v != "" {
		c.Server.MasterKey = v
	}
	if v := os.Getenv("LLMUX_INSECURE_KEYLESS"); v != "" {
		c.Server.InsecureKeyless = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("LLMUX_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("LLMUX_USAGE_LOG"); v != "" {
		c.UsageLogPath = v
	}
	// Postgres DSN resolution (later wins). LLMUX_POSTGRES is the legacy,
	// product-specific var and remains a working fallback. DATABASE_URL is the
	// standard shared DSN; VULOS_DATABASE_URL is the Vulos-specific shared DSN
	// and wins over both so a deployment can point llmux at a different database
	// than a generic DATABASE_URL when needed. Any shared DSN is preferred over
	// LLMUX_POSTGRES (cloud consolidation onto one Neon database).
	if v := os.Getenv("LLMUX_POSTGRES"); v != "" {
		c.Postgres = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		c.Postgres = v
	}
	if v := os.Getenv("VULOS_DATABASE_URL"); v != "" {
		c.Postgres = v
	}
	if v := os.Getenv("LLMUX_POSTGRES_SCHEMA"); v != "" {
		c.PostgresSchema = v
	}
	// Whenever Postgres is in play, default the schema to "llmux" so tables live
	// in a dedicated namespace and never collide with other products sharing the
	// database.
	if c.Postgres != "" && c.PostgresSchema == "" {
		c.PostgresSchema = "llmux"
	}
	if v := os.Getenv("LLMUX_REDIS"); v != "" {
		c.Redis = v
	}
	if v := os.Getenv("LLMUX_SYNC_INTERVAL_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pricing.SyncIntervalMinutes = n
		}
	}
	if v := os.Getenv("LLMUX_CP_URL"); v != "" {
		c.CP.URL = v
	}
	if v := os.Getenv("LLMUX_CP_SECRET"); v != "" {
		c.CP.SharedSecret = v
	}
	if v := os.Getenv("LLMUX_CP_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.CP.RPM = n
		}
	}
	if v := os.Getenv("LLMUX_CP_ENTITLEMENT_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.CP.EntitlementTTLSeconds = n
		}
	}
	if v := os.Getenv("LLMUX_CP_DEGRADED_FAIL_OPEN"); v != "" {
		c.CP.DegradedFailOpen = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("LLMUX_CP_DEGRADED_RPM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.CP.DegradedRPM = n
		}
	}
	if v := os.Getenv("LLMUX_CP_USAGE_SPOOL_PATH"); v != "" {
		c.CP.UsageSpoolPath = v
	}
	if v := os.Getenv("LLMUX_BYOK_KEK"); v != "" {
		c.BYOK.KEK = v
	}
	if v := os.Getenv("LLMUX_BYOK_STORE"); v != "" {
		c.BYOK.StorePath = v
	}
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	if c.Server.Addr == "" && c.Server.SocketPath == "" {
		return fmt.Errorf("server: one of addr or socket_path must be set")
	}
	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider: name is required")
		}
		if names[p.Name] {
			return fmt.Errorf("provider: duplicate name %q", p.Name)
		}
		names[p.Name] = true
		switch p.Type {
		case TypePassthrough, TypeAnthropic, TypeGemini, TypeCohere, TypeBedrock, TypeAzure:
		default:
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
		switch p.Tier {
		case "", "local", "sovereign", "brokered", "external":
		default:
			return fmt.Errorf("provider %q: invalid tier %q (want one of local, sovereign, brokered, external, or empty)", p.Name, p.Tier)
		}
	}
	for _, r := range c.Routes {
		if r.Provider != "" && !names[r.Provider] {
			return fmt.Errorf("route %q: unknown provider %q", r.Model, r.Provider)
		}
	}
	return nil
}

// ProviderByName returns the named provider config, or false.
func (c *Config) ProviderByName(name string) (ProviderConfig, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// String renders a redacted summary (never prints secrets).
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "llmux config: addr=%q socket=%q providers=[", c.Server.Addr, c.Server.SocketPath)
	for i, p := range c.Providers {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s(%s)", p.Name, p.Type)
	}
	b.WriteString("]")
	return b.String()
}

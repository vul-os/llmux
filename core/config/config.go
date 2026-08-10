// Package config loads llmux configuration from a JSON file and environment
// variables. It is dependency-free (stdlib only) so the core always builds.
//
// Resolution order for the sidecar (Load): built-in defaults -> config file ->
// env vars, later wins. For embedders (FromJSON) the caller's document wins over
// the environment and the host's un-namespaced DSN variables are ignored — see
// envPolicy for why the two differ.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
	// Setting it is not free: the Postgres key store connects and MIGRATES
	// eagerly at gateway.New (CREATE SCHEMA / CREATE TABLE), so this is the one
	// field that turns an otherwise inert construction into remote I/O.
	//
	// How the DSN is resolved depends on who is asking (see envPolicy):
	//
	//   Load / `llmux serve`, an operator's config file — later wins:
	//     this field -> LLMUX_POSTGRES -> DATABASE_URL -> VULOS_DATABASE_URL
	//     (the shared Neon DSN; preferred for cloud consolidation).
	//
	//   FromJSON / library and C-ABI embedders — the document wins:
	//     this field, else LLMUX_POSTGRES. DATABASE_URL and VULOS_DATABASE_URL
	//     are the HOST application's variables and are not read: a library must
	//     not migrate someone else's production database because it was loaded
	//     into their process.
	//
	// When Postgres is in use, all llmux tables live under a dedicated schema
	// (PostgresSchema, default "llmux") so llmux can share one database with the
	// other Vulos products without name collisions.
	Postgres string `json:"postgres"`
	// PostgresSchema is the Postgres schema that holds llmux's tables. It lets
	// llmux share one database (e.g. a single Neon database) with other products.
	// Empty defaults to "llmux" whenever Postgres is set. Resolved from env
	// LLMUX_POSTGRES_SCHEMA.
	PostgresSchema string `json:"postgres_schema"`
	// PostgresConnectTimeoutSeconds bounds the eager connect-and-migrate that
	// gateway.New performs when Postgres is configured. 0 selects the built-in
	// default (DefaultPostgresConnectTimeoutSeconds); a negative value means no
	// deadline, which is what this used to be unconditionally — a context.Background()
	// that could park the calling thread forever against a black-holed DSN. In
	// library mode that thread belongs to the host. Resolved from env
	// LLMUX_POSTGRES_CONNECT_TIMEOUT_SECONDS.
	PostgresConnectTimeoutSeconds int `json:"postgres_connect_timeout_seconds"`
	// Redis address (host:port). When set, rate limiting and (if caching is
	// enabled) the response cache use Redis — correct across replicas.
	Redis string `json:"redis"`

	// UpstreamTimeoutSeconds bounds a single non-streaming upstream call
	// (0 = no extra deadline beyond the client default).
	UpstreamTimeoutSeconds int `json:"upstream_timeout_seconds"`

	// StreamFirstByteTimeoutSeconds bounds how long a streaming call waits for
	// the FIRST chunk. 0 selects DefaultStreamFirstByteTimeoutSeconds; negative
	// disables it.
	//
	// A streaming response deliberately has no wall-clock deadline — a long
	// generation is a correct stream, not a hung one, and a total timeout would
	// truncate it. The two bounds that CAN distinguish "still working" from
	// "gone" are time-to-first-chunk and the gap between chunks, so those are
	// what llmux enforces. Before this there was neither, and a streaming call
	// against an upstream that accepted the connection and then said nothing
	// blocked its caller forever: a request goroutine in the sidecar, a host
	// thread in library mode.
	StreamFirstByteTimeoutSeconds int `json:"stream_first_byte_timeout_seconds"`
	// StreamIdleTimeoutSeconds bounds the gap BETWEEN chunks once a stream has
	// begun. 0 selects DefaultStreamIdleTimeoutSeconds; negative disables it.
	// The clock restarts on every chunk, so a stream that keeps producing is
	// never cut off however long it runs in total.
	StreamIdleTimeoutSeconds int `json:"stream_idle_timeout_seconds"`
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

	// CP optionally points llmux at an external control plane ("cp")
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
	// URL is the cp base URL (e.g. https://cp.vulos.org). Empty = standalone.
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
	//
	// EGRESS NOTE: Default() populates this with two public price feeds, so a
	// stock gateway makes an outbound GET at startup and every
	// SyncIntervalMinutes. That fetch carries no prompt, no key and no usage —
	// it is a plain GET of a public price list — but it is off-box traffic that
	// the sovereignty gate does not classify (the gate governs inference
	// dispatch). Set "sources": [] to disable it; the built-in seed catalog
	// still prices requests offline.
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

// Built-in timeout defaults. They are exported and applied at the point of use
// (not only in Default) so a hand-built Config — a Go embedder's literal, a
// test's fixture — is bounded too rather than inheriting "forever" from a zero
// value.
//
// The numbers: 30s to connect and migrate a Postgres store is generous for any
// reachable database and short enough that a host notices a wrong DSN as an
// error instead of a hang. 60s to the first streamed chunk covers a cold model
// load or a long queue at a busy provider, and 120s between chunks is far
// beyond any real inter-token gap while still catching a dead connection whose
// FIN never arrived. All three are configurable, and a negative value opts out.
const (
	DefaultPostgresConnectTimeoutSeconds = 30
	DefaultStreamFirstByteTimeoutSeconds = 60
	DefaultStreamIdleTimeoutSeconds      = 120
)

// resolveTimeout maps the 0-means-default / negative-means-off convention onto
// a duration. Zero return means "no timeout".
func resolveTimeout(configured, fallback int) time.Duration {
	switch {
	case configured < 0:
		return 0
	case configured == 0:
		return time.Duration(fallback) * time.Second
	default:
		return time.Duration(configured) * time.Second
	}
}

// PostgresConnectTimeout is the deadline for gateway.New's eager connect and
// migrate. Zero means no deadline.
func (c *Config) PostgresConnectTimeout() time.Duration {
	return resolveTimeout(c.PostgresConnectTimeoutSeconds, DefaultPostgresConnectTimeoutSeconds)
}

// StreamFirstByteTimeout is how long a streaming call waits for its first
// chunk. Zero means no deadline.
func (c *Config) StreamFirstByteTimeout() time.Duration {
	return resolveTimeout(c.StreamFirstByteTimeoutSeconds, DefaultStreamFirstByteTimeoutSeconds)
}

// StreamIdleTimeout is the maximum gap between chunks of a running stream. Zero
// means no deadline.
func (c *Config) StreamIdleTimeout() time.Duration {
	return resolveTimeout(c.StreamIdleTimeoutSeconds, DefaultStreamIdleTimeoutSeconds)
}

// Default returns a config with sane defaults and providers auto-detected from
// well-known environment variables, so llmux works out of the box.
func Default() *Config {
	c := &Config{
		Server:   ServerConfig{Addr: ":4000"},
		LogLevel: "info",
		Retry:    RetryConfig{MaxRetries: 2, BackoffMS: 200},
		Cache:    CacheConfig{MaxEntries: 10000},

		PostgresConnectTimeoutSeconds: DefaultPostgresConnectTimeoutSeconds,
		StreamFirstByteTimeoutSeconds: DefaultStreamFirstByteTimeoutSeconds,
		StreamIdleTimeoutSeconds:      DefaultStreamIdleTimeoutSeconds,
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
	var data []byte
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			data = b
		case os.IsNotExist(err):
			// A missing config file is not an error: defaults + env stand alone.
		default:
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	c, err := fromJSON(data, operatorEnv())
	if err != nil {
		if path != "" {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
		return nil, err
	}
	return c, nil
}

// FromJSON builds the configuration from defaults, the given JSON document
// (empty or nil is allowed and means "defaults only"), then environment
// overrides.
//
// It exists because not every host has a config FILE. The C-ABI layer in ffi/
// receives its configuration as a JSON string across the boundary, and an
// in-process Go host may hold one in memory; both need Load's semantics —
// defaults merged, env applied, validated — rather than a bare json.Unmarshal
// into a zero Config, which would silently drop every default (no retries, no
// cache bound, no auto-detected local backend) and skip Validate entirely.
//
// It is NOT identical to Load, and the difference is who owns the environment.
// This is the LIBRARY entry point: the caller is an application that embedded
// llmux, and the process environment is that application's, not llmux's. So it
// resolves under embeddedEnv (see envPolicy):
//
//   - a value the document states explicitly WINS over the environment, and
//   - the un-namespaced shared DSNs (DATABASE_URL, VULOS_DATABASE_URL) are not
//     read at all.
//
// Load, which an operator invokes with a config file they wrote for the llmux
// sidecar, keeps the historical operatorEnv rules (env last, all three DSN
// variables honoured). See envPolicy for the full reasoning.
func FromJSON(data []byte) (*Config, error) {
	return fromJSON(data, embeddedEnv())
}

func fromJSON(data []byte, pol envPolicy) (*Config, error) {
	c := Default()
	if len(bytes.TrimSpace(data)) > 0 {
		// A document may omit providers; only overwrite slices it provides.
		fileCfg := &Config{}
		if err := json.Unmarshal(data, fileCfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		c.merge(fileCfg)
		if pol.documentWins {
			set, err := statedKeys(data)
			if err != nil {
				return nil, err
			}
			pol.stated = set
		}
	}
	c.applyEnv(pol)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Environment policy
// ---------------------------------------------------------------------------

// envPolicy says how much of the process environment a given caller's
// configuration is allowed to absorb.
//
// The two policies exist because "the environment" means two different things
// depending on who called.
//
// operatorEnv is the sidecar: `llmux serve` reading a file the operator wrote,
// in a process the operator started, whose variables the operator set for
// llmux. Env-overrides-file is a deliberate deployment pattern there (one image,
// per-environment variables) and it stays exactly as it always was.
//
// embeddedEnv is library mode: libllmux inside a Rails, Django or JVM process,
// or core/gateway inside another Go binary. The host's environment was set for
// the HOST. Two consequences:
//
//  1. The caller's document is the only thing llmux was actually handed, so a
//     value stated in it wins over any variable. Previously applyEnv ran AFTER
//     the merge and overwrote it unconditionally.
//  2. DATABASE_URL and VULOS_DATABASE_URL are not llmux's names. They are the
//     host application's own DSN, or the deployment's, and llmux happens to
//     read them. In a Rails app with DATABASE_URL set, llmux_new used to adopt
//     the app's production database and CREATE SCHEMA/CREATE TABLE in it (the
//     Postgres key store connects and migrates eagerly), while llmux.h promised
//     the gateway was inert "unless your configuration names a Postgres DSN".
//     A library does not get to reinterpret the host's variables as an
//     instruction to migrate their database, so the un-namespaced names are
//     ignored here. LLMUX_POSTGRES is namespaced and unambiguous — setting it
//     can only have been meant for llmux — so it is still honoured, and an
//     embedder can always state "postgres" in the document.
type envPolicy struct {
	// documentWins makes a value stated in the caller's JSON document beat the
	// environment. When false, env is applied last (the historical order).
	documentWins bool
	// sharedDSN allows DATABASE_URL / VULOS_DATABASE_URL to name the Postgres
	// DSN. When false only LLMUX_POSTGRES can.
	sharedDSN bool
	// stated holds the dotted JSON paths the caller's document set explicitly
	// (e.g. "postgres", "server.addr", "cp.cp_url"). Only consulted when
	// documentWins.
	stated map[string]bool
}

// operatorEnv is the policy for `llmux serve` reading an operator's config file.
func operatorEnv() envPolicy { return envPolicy{documentWins: false, sharedDSN: true} }

// embeddedEnv is the policy for a host that embedded llmux and handed it a
// configuration document.
func embeddedEnv() envPolicy { return envPolicy{documentWins: true, sharedDSN: false} }

// locked reports whether the caller's document stated this key, in which case
// the environment must not overwrite it.
func (p envPolicy) locked(key string) bool {
	return p.documentWins && p.stated[key]
}

// statedKeys returns the dotted paths of the leaves the document set. Only the
// leaves applyEnv can touch matter, so it decodes one level of the four nested
// blocks env reaches into (server, pricing, cp, byok) and treats everything
// else as a top-level key.
//
// It reads the RAW document rather than inspecting the decoded Config because
// the two are not the same question: a decoded `"postgres": ""` and an absent
// "postgres" are both the empty string, and only the raw form can tell "the
// caller said nothing, fill it from the environment" from "the caller said
// explicitly: no database".
func statedKeys(data []byte) (map[string]bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	out := make(map[string]bool, len(top))
	nested := map[string]bool{"server": true, "pricing": true, "cp": true, "byok": true}
	for k, v := range top {
		out[k] = true
		if !nested[k] {
			continue
		}
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(v, &sub); err != nil {
			// A non-object where an object belongs is the outer Unmarshal's
			// problem to report; here it just means no stated leaves.
			continue
		}
		for sk := range sub {
			out[k+"."+sk] = true
		}
	}
	return out, nil
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
	// nil means "the file said nothing about sources" (keep the defaults); an
	// explicitly-empty list means "no price feeds", which is the only way an
	// operator can turn off the ONE outbound call a stock gateway makes on its
	// own (see PricingConfig.Sources). A len>0 check would silently ignore
	// "sources": [] and leave the default feeds dialing.
	if o.Pricing.Sources != nil {
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
	if o.PostgresConnectTimeoutSeconds != 0 {
		c.PostgresConnectTimeoutSeconds = o.PostgresConnectTimeoutSeconds
	}
	if o.StreamFirstByteTimeoutSeconds != 0 {
		c.StreamFirstByteTimeoutSeconds = o.StreamFirstByteTimeoutSeconds
	}
	if o.StreamIdleTimeoutSeconds != 0 {
		c.StreamIdleTimeoutSeconds = o.StreamIdleTimeoutSeconds
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

func (c *Config) applyEnv(pol envPolicy) {
	// env applies a variable unless the caller's document already stated that
	// key. See envPolicy: under operatorEnv nothing is locked and this is the
	// historical env-wins behaviour.
	env := func(key, name string, set func(string)) {
		if pol.locked(key) {
			return
		}
		if v := os.Getenv(name); v != "" {
			set(v)
		}
	}

	env("server.addr", "LLMUX_ADDR", func(v string) { c.Server.Addr = v })
	env("server.socket_path", "LLMUX_SOCKET", func(v string) { c.Server.SocketPath = v })
	env("server.master_key", "LLMUX_MASTER_KEY", func(v string) { c.Server.MasterKey = v })
	env("server.insecure_keyless", "LLMUX_INSECURE_KEYLESS", func(v string) {
		c.Server.InsecureKeyless = v == "1" || strings.EqualFold(v, "true")
	})
	env("log_level", "LLMUX_LOG_LEVEL", func(v string) { c.LogLevel = v })
	env("usage_log_path", "LLMUX_USAGE_LOG", func(v string) { c.UsageLogPath = v })

	// Postgres DSN resolution (later wins). LLMUX_POSTGRES is the namespaced
	// variable and is always honoured — nobody sets it by accident. DATABASE_URL
	// is the standard shared DSN; VULOS_DATABASE_URL is the Vulos-specific shared
	// DSN and wins over both so a deployment can point llmux at a different
	// database than a generic DATABASE_URL when needed. Any shared DSN is
	// preferred over LLMUX_POSTGRES (cloud consolidation onto one Neon database).
	//
	// The two shared names are read only under operatorEnv. In library mode they
	// belong to the host application, and adopting them means running
	// CREATE SCHEMA / CREATE TABLE inside someone else's production database
	// because they loaded a shared library. See envPolicy.
	env("postgres", "LLMUX_POSTGRES", func(v string) { c.Postgres = v })
	if pol.sharedDSN {
		env("postgres", "DATABASE_URL", func(v string) { c.Postgres = v })
		env("postgres", "VULOS_DATABASE_URL", func(v string) { c.Postgres = v })
	}
	env("postgres_schema", "LLMUX_POSTGRES_SCHEMA", func(v string) { c.PostgresSchema = v })
	env("postgres_connect_timeout_seconds", "LLMUX_POSTGRES_CONNECT_TIMEOUT_SECONDS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.PostgresConnectTimeoutSeconds = n
		}
	})
	env("stream_first_byte_timeout_seconds", "LLMUX_STREAM_FIRST_BYTE_TIMEOUT_SECONDS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.StreamFirstByteTimeoutSeconds = n
		}
	})
	env("stream_idle_timeout_seconds", "LLMUX_STREAM_IDLE_TIMEOUT_SECONDS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.StreamIdleTimeoutSeconds = n
		}
	})
	// Whenever Postgres is in play, default the schema to "llmux" so tables live
	// in a dedicated namespace and never collide with other products sharing the
	// database.
	if c.Postgres != "" && c.PostgresSchema == "" {
		c.PostgresSchema = "llmux"
	}
	env("redis", "LLMUX_REDIS", func(v string) { c.Redis = v })
	env("pricing.sync_interval_minutes", "LLMUX_SYNC_INTERVAL_MIN", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pricing.SyncIntervalMinutes = n
		}
	})
	env("cp.cp_url", "LLMUX_CP_URL", func(v string) { c.CP.URL = v })
	env("cp.cp_shared_secret", "LLMUX_CP_SECRET", func(v string) { c.CP.SharedSecret = v })
	env("cp.cp_rpm", "LLMUX_CP_RPM", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.CP.RPM = n
		}
	})
	env("cp.cp_entitlement_ttl_seconds", "LLMUX_CP_ENTITLEMENT_TTL_SECONDS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.CP.EntitlementTTLSeconds = n
		}
	})
	env("cp.cp_degraded_fail_open", "LLMUX_CP_DEGRADED_FAIL_OPEN", func(v string) {
		c.CP.DegradedFailOpen = v == "1" || strings.EqualFold(v, "true")
	})
	env("cp.cp_degraded_rpm", "LLMUX_CP_DEGRADED_RPM", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			c.CP.DegradedRPM = n
		}
	})
	env("cp.cp_usage_spool_path", "LLMUX_CP_USAGE_SPOOL_PATH", func(v string) { c.CP.UsageSpoolPath = v })
	env("byok.kek", "LLMUX_BYOK_KEK", func(v string) { c.BYOK.KEK = v })
	env("byok.store_path", "LLMUX_BYOK_STORE", func(v string) { c.BYOK.StorePath = v })
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

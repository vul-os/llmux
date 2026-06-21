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
	Postgres string `json:"postgres"`
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

	// CP optionally points llmux at a Vulos control plane ("cp" / vulos-cloud)
	// for central identity/budget/usage. Empty = standalone (the default): the
	// gateway uses its static keys and never talks to cp. This config is read by
	// the composition root (cmd/llmux) to wire the OPTIONAL integration/cp
	// adapter; the core gateway never imports it.
	CP CPConfig `json:"cp"`
}

// CPConfig configures the optional control-plane integration.
type CPConfig struct {
	// URL is the cp base URL (e.g. https://cp.vulos.to). Empty = standalone.
	URL string `json:"cp_url"`
	// SharedSecret authenticates outbound cp calls via the X-Relay-Auth header.
	SharedSecret string `json:"cp_shared_secret"`
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
	for _, kp := range knownProviders {
		if os.Getenv(kp.env) == "" {
			continue
		}
		c.Providers = append(c.Providers, ProviderConfig{
			Name: kp.name, Type: kp.typ, BaseURL: kp.baseURL, APIKeyEnv: kp.env,
		})
	}
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
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
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
	if v := os.Getenv("LLMUX_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("LLMUX_POSTGRES"); v != "" {
		c.Postgres = v
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

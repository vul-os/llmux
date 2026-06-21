package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearKnownProviderEnv unsets every env var that autoDetectProviders looks at,
// so tests start from a clean slate regardless of the host environment.
func clearKnownProviderEnv(t *testing.T) {
	t.Helper()
	for _, kp := range knownProviders {
		t.Setenv(kp.env, "")
		os.Unsetenv(kp.env)
	}
}

// clearLLMUXEnv unsets the LLMUX_* overrides applyEnv reads.
func clearLLMUXEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LLMUX_ADDR", "LLMUX_SOCKET", "LLMUX_MASTER_KEY", "LLMUX_LOG_LEVEL",
		"LLMUX_POSTGRES", "LLMUX_REDIS", "LLMUX_SYNC_INTERVAL_MIN",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestDefault(t *testing.T) {
	clearKnownProviderEnv(t)
	c := Default()

	if c.Server.Addr != ":4000" {
		t.Errorf("Addr = %q, want :4000", c.Server.Addr)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", c.LogLevel)
	}
	if c.Retry.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", c.Retry.MaxRetries)
	}
	if c.Retry.BackoffMS != 200 {
		t.Errorf("BackoffMS = %d, want 200", c.Retry.BackoffMS)
	}
	if c.Cache.MaxEntries != 10000 {
		t.Errorf("Cache.MaxEntries = %d, want 10000", c.Cache.MaxEntries)
	}
	if c.Pricing.SyncIntervalMinutes != 360 {
		t.Errorf("SyncIntervalMinutes = %d, want 360", c.Pricing.SyncIntervalMinutes)
	}
	if len(c.Pricing.Sources) != 2 {
		t.Errorf("Pricing.Sources len = %d, want 2", len(c.Pricing.Sources))
	}
	if len(c.Providers) != 0 {
		t.Errorf("Providers should be empty with no env set, got %d", len(c.Providers))
	}
}

func TestAutoDetectProviders(t *testing.T) {
	clearKnownProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic")
	t.Setenv("GEMINI_API_KEY", "sk-gemini")
	t.Setenv("COHERE_API_KEY", "sk-cohere")

	c := Default()

	want := map[string]ProviderType{
		"openai":    TypePassthrough,
		"anthropic": TypeAnthropic,
		"gemini":    TypeGemini,
		"cohere":    TypeCohere,
	}
	if len(c.Providers) != len(want) {
		t.Fatalf("got %d providers, want %d: %v", len(c.Providers), len(want), c.Providers)
	}
	for _, p := range c.Providers {
		typ, ok := want[p.Name]
		if !ok {
			t.Errorf("unexpected provider %q", p.Name)
			continue
		}
		if p.Type != typ {
			t.Errorf("provider %q type = %q, want %q", p.Name, p.Type, typ)
		}
		if p.APIKeyEnv == "" {
			t.Errorf("provider %q should reference an env var", p.Name)
		}
		if p.BaseURL == "" {
			t.Errorf("provider %q should have a base URL", p.Name)
		}
	}
}

func TestAutoDetectProviders_Unset(t *testing.T) {
	clearKnownProviderEnv(t)
	// Only set one; the rest must be absent.
	t.Setenv("GROQ_API_KEY", "gsk-1")

	c := Default()
	if len(c.Providers) != 1 {
		t.Fatalf("want exactly 1 provider, got %d: %v", len(c.Providers), c.Providers)
	}
	if _, ok := c.ProviderByName("groq"); !ok {
		t.Errorf("expected groq provider present")
	}
	if _, ok := c.ProviderByName("openai"); ok {
		t.Errorf("openai should be absent when OPENAI_API_KEY unset")
	}
}

func TestLoad_NonexistentPath(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load nonexistent path returned error: %v", err)
	}
	if c.Server.Addr != ":4000" {
		t.Errorf("Addr = %q, want default :4000", c.Server.Addr)
	}
}

func TestLoad_EmptyPath(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load empty path returned error: %v", err)
	}
	if c.Server.Addr != ":4000" {
		t.Errorf("Addr = %q, want default :4000", c.Server.Addr)
	}
}

func writeTempConfig(t *testing.T, c *Config) string {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_FileOverridesAndMerges(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	file := &Config{
		Server: ServerConfig{Addr: ":9999", MasterKey: "master"},
		Providers: []ProviderConfig{
			{Name: "p1", Type: TypePassthrough, BaseURL: "https://x/v1", APIKey: "k"},
		},
		Routes: []RouteConfig{
			{Model: "gpt-x", Provider: "p1"},
		},
		Retry:                  RetryConfig{MaxRetries: 5, BackoffMS: 1000},
		Cache:                  CacheConfig{Enabled: true, TTLSeconds: 30, MaxEntries: 7},
		Keys:                   []KeyConfig{{Key: "vk", Name: "team", BudgetUSD: 10, RPM: 60}},
		Postgres:               "postgres://file",
		Redis:                  "file-redis:6379",
		KeyStorePath:           "/tmp/keys.json",
		UpstreamTimeoutSeconds: 42,
		MaxResponseBytes:       1234,
		Pricing:                PricingConfig{CatalogPath: "/tmp/cat", SyncIntervalMinutes: 99},
	}
	path := writeTempConfig(t, file)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.Server.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", c.Server.Addr)
	}
	if c.Server.MasterKey != "master" {
		t.Errorf("MasterKey = %q, want master", c.Server.MasterKey)
	}
	if len(c.Providers) != 1 || c.Providers[0].Name != "p1" {
		t.Errorf("Providers = %v, want single p1", c.Providers)
	}
	if len(c.Routes) != 1 || c.Routes[0].Model != "gpt-x" {
		t.Errorf("Routes = %v, want single gpt-x", c.Routes)
	}
	if c.Retry.MaxRetries != 5 || c.Retry.BackoffMS != 1000 {
		t.Errorf("Retry = %+v, want {5 1000}", c.Retry)
	}
	if !c.Cache.Enabled || c.Cache.TTLSeconds != 30 || c.Cache.MaxEntries != 7 {
		t.Errorf("Cache = %+v, want enabled ttl=30 max=7", c.Cache)
	}
	if len(c.Keys) != 1 || c.Keys[0].Key != "vk" {
		t.Errorf("Keys = %v, want single vk", c.Keys)
	}
	if c.Postgres != "postgres://file" {
		t.Errorf("Postgres = %q", c.Postgres)
	}
	if c.Redis != "file-redis:6379" {
		t.Errorf("Redis = %q", c.Redis)
	}
	if c.KeyStorePath != "/tmp/keys.json" {
		t.Errorf("KeyStorePath = %q", c.KeyStorePath)
	}
	if c.UpstreamTimeoutSeconds != 42 {
		t.Errorf("UpstreamTimeoutSeconds = %d, want 42", c.UpstreamTimeoutSeconds)
	}
	if c.MaxResponseBytes != 1234 {
		t.Errorf("MaxResponseBytes = %d, want 1234", c.MaxResponseBytes)
	}
	if c.Pricing.CatalogPath != "/tmp/cat" || c.Pricing.SyncIntervalMinutes != 99 {
		t.Errorf("Pricing = %+v", c.Pricing)
	}
}

func TestLoad_FilePartialMergeKeepsDefaults(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	// A file that omits most fields must keep defaults for those.
	path := filepath.Join(t.TempDir(), "partial.json")
	if err := os.WriteFile(path, []byte(`{"log_level":"debug","providers":[{"name":"only","type":"passthrough"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", c.LogLevel)
	}
	// Defaults preserved.
	if c.Server.Addr != ":4000" {
		t.Errorf("Addr = %q, want default :4000", c.Server.Addr)
	}
	if c.Retry.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want default 2", c.Retry.MaxRetries)
	}
	if c.Cache.MaxEntries != 10000 {
		t.Errorf("Cache.MaxEntries = %d, want default 10000", c.Cache.MaxEntries)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error for invalid JSON")
	}
}

func TestLoad_EnvOverridesFile(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	file := &Config{
		Server:   ServerConfig{Addr: ":9999", MasterKey: "file-master"},
		LogLevel: "warn",
		Postgres: "postgres://file",
		Redis:    "file-redis:6379",
		Providers: []ProviderConfig{
			{Name: "p1", Type: TypePassthrough},
		},
	}
	path := writeTempConfig(t, file)

	t.Setenv("LLMUX_ADDR", ":1234")
	t.Setenv("LLMUX_MASTER_KEY", "env-master")
	t.Setenv("LLMUX_POSTGRES", "postgres://env")
	t.Setenv("LLMUX_REDIS", "env-redis:6380")
	t.Setenv("LLMUX_LOG_LEVEL", "error")
	t.Setenv("LLMUX_SYNC_INTERVAL_MIN", "12")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.Server.Addr != ":1234" {
		t.Errorf("Addr = %q, want env :1234", c.Server.Addr)
	}
	if c.Server.MasterKey != "env-master" {
		t.Errorf("MasterKey = %q, want env-master", c.Server.MasterKey)
	}
	if c.Postgres != "postgres://env" {
		t.Errorf("Postgres = %q, want env value", c.Postgres)
	}
	if c.Redis != "env-redis:6380" {
		t.Errorf("Redis = %q, want env value", c.Redis)
	}
	if c.LogLevel != "error" {
		t.Errorf("LogLevel = %q, want error", c.LogLevel)
	}
	if c.Pricing.SyncIntervalMinutes != 12 {
		t.Errorf("SyncIntervalMinutes = %d, want 12", c.Pricing.SyncIntervalMinutes)
	}
}

func TestApplyEnv_SocketAndBadSyncInterval(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)

	t.Setenv("LLMUX_SOCKET", "/tmp/llmux.sock")
	t.Setenv("LLMUX_SYNC_INTERVAL_MIN", "not-a-number")

	c := Default()
	c.applyEnv()

	if c.Server.SocketPath != "/tmp/llmux.sock" {
		t.Errorf("SocketPath = %q", c.Server.SocketPath)
	}
	// Bad integer should be ignored, leaving the default.
	if c.Pricing.SyncIntervalMinutes != 360 {
		t.Errorf("SyncIntervalMinutes = %d, want unchanged 360", c.Pricing.SyncIntervalMinutes)
	}
}

func TestValidate_NoListener(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error when neither addr nor socket set")
	}
}

func TestValidate_SocketOnlyOK(t *testing.T) {
	c := &Config{Server: ServerConfig{SocketPath: "/tmp/s.sock"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("socket-only config should be valid: %v", err)
	}
}

func TestValidate_EmptyProviderName(t *testing.T) {
	c := &Config{
		Server:    ServerConfig{Addr: ":4000"},
		Providers: []ProviderConfig{{Name: "", Type: TypePassthrough}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for empty provider name")
	}
}

func TestValidate_DuplicateProviderName(t *testing.T) {
	c := &Config{
		Server: ServerConfig{Addr: ":4000"},
		Providers: []ProviderConfig{
			{Name: "dup", Type: TypePassthrough},
			{Name: "dup", Type: TypeAnthropic},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate name error, got %v", err)
	}
}

func TestValidate_UnknownProviderType(t *testing.T) {
	c := &Config{
		Server:    ServerConfig{Addr: ":4000"},
		Providers: []ProviderConfig{{Name: "p", Type: ProviderType("nope")}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected unknown type error, got %v", err)
	}
}

func TestValidate_RouteUnknownProvider(t *testing.T) {
	c := &Config{
		Server:    ServerConfig{Addr: ":4000"},
		Providers: []ProviderConfig{{Name: "p1", Type: TypePassthrough}},
		Routes:    []RouteConfig{{Model: "m", Provider: "ghost"}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("expected unknown provider error, got %v", err)
	}
}

func TestValidate_Success(t *testing.T) {
	c := &Config{
		Server: ServerConfig{Addr: ":4000"},
		Providers: []ProviderConfig{
			{Name: "p1", Type: TypePassthrough},
			{Name: "p2", Type: TypeAnthropic},
		},
		Routes: []RouteConfig{
			{Model: "m", Provider: "p1"},
			{Model: "*", Provider: ""}, // empty provider is allowed (catch-all w/ strategy)
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestResolveKey(t *testing.T) {
	// Direct key wins.
	p := ProviderConfig{APIKey: "direct", APIKeyEnv: "SHOULD_NOT_READ"}
	if got := p.ResolveKey(); got != "direct" {
		t.Errorf("ResolveKey direct = %q, want direct", got)
	}

	// Env-backed key.
	t.Setenv("MY_PROVIDER_KEY", "from-env")
	p2 := ProviderConfig{APIKeyEnv: "MY_PROVIDER_KEY"}
	if got := p2.ResolveKey(); got != "from-env" {
		t.Errorf("ResolveKey env = %q, want from-env", got)
	}

	// Neither set.
	p3 := ProviderConfig{}
	if got := p3.ResolveKey(); got != "" {
		t.Errorf("ResolveKey empty = %q, want empty", got)
	}

	// Env var named but unset -> empty.
	p4 := ProviderConfig{APIKeyEnv: "DEFINITELY_UNSET_VAR_XYZ"}
	os.Unsetenv("DEFINITELY_UNSET_VAR_XYZ")
	if got := p4.ResolveKey(); got != "" {
		t.Errorf("ResolveKey unset env = %q, want empty", got)
	}
}

func TestProviderByName(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{
		{Name: "alpha", Type: TypePassthrough},
		{Name: "beta", Type: TypeAnthropic},
	}}
	got, ok := c.ProviderByName("beta")
	if !ok {
		t.Fatal("expected beta found")
	}
	if got.Type != TypeAnthropic {
		t.Errorf("beta type = %q, want anthropic", got.Type)
	}
	if _, ok := c.ProviderByName("missing"); ok {
		t.Error("expected missing to not be found")
	}
}

func TestString_NoSecrets(t *testing.T) {
	c := &Config{
		Server: ServerConfig{Addr: ":4000", SocketPath: "/tmp/s", MasterKey: "super-secret-master"},
		Providers: []ProviderConfig{
			{Name: "p1", Type: TypePassthrough, APIKey: "sk-super-secret-key"},
			{Name: "p2", Type: TypeAnthropic, APIKeyEnv: "ENV_NAME_SECRET"},
		},
	}
	s := c.String()

	for _, secret := range []string{"sk-super-secret-key", "super-secret-master"} {
		if strings.Contains(s, secret) {
			t.Errorf("String() leaked secret %q: %s", secret, s)
		}
	}
	// Should still mention provider names and types.
	if !strings.Contains(s, "p1") || !strings.Contains(s, "passthrough") {
		t.Errorf("String() missing provider summary: %s", s)
	}
	if !strings.Contains(s, ":4000") {
		t.Errorf("String() missing addr: %s", s)
	}
}

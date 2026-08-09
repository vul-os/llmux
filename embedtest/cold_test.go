package embedtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
)

// G2 — cold construction is inert.
//
// The regression this guards is concrete and shipped once already: the pricing
// syncer quietly GET-ing openrouter.ai every six hours from a process that only
// ever wanted an in-process Chat call. "New returned no error" does not detect
// that; only counting goroutines and round trips does.
//
// TWO HONEST EXCEPTIONS, guarded as they really are rather than as the slogan
// says (a guard asserting something false gets weakened until it checks
// nothing, which is how this suite accumulated its false greens):
//
//  1. New DOES connect when cfg.Postgres is set — keys.NewPGStore pings and
//     migrates eagerly. So the zero-round-trip claim is asserted for a config
//     with NO Postgres DSN (every default and every library config), and the
//     eager connect is asserted separately, by name, in
//     TestPostgresDSNConnectsEagerlyAtConstruction.
//  2. New DOES read the environment for a provider whose config NAMES an env
//     var (config.ProviderConfig.ResolveKey → os.Getenv(APIKeyEnv)). That is
//     config-directed, not auto-detection. The real property — New never adopts
//     a provider key the config did not name — is what
//     TestConstructionAdoptsNoUnnamedProviderKey asserts.

// settle samples runtime.NumGoroutine() until it stops moving, so the count is
// not read while an unrelated goroutine is still winding down. A bare sleep
// would be a coin flip in both directions: too short and it flakes red, too
// long and it hides a goroutine that exits after 100ms.
func settle(t *testing.T) int {
	t.Helper()
	const stableSamples = 8
	deadline := time.Now().Add(3 * time.Second)
	last, streak := -1, 0
	for time.Now().Before(deadline) {
		runtime.Gosched()
		n := runtime.NumGoroutine()
		if n == last {
			if streak++; streak >= stableSamples {
				return n
			}
		} else {
			last, streak = n, 0
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Never settling makes the measurement meaningless. An inconclusive run must
	// not be reported as a pass.
	t.Fatalf("goroutine count never settled within 3s (last=%d) — this guard could not measure anything", last)
	return 0
}

func goroutineDump() string {
	buf := make([]byte, 1<<16)
	return string(buf[:runtime.Stack(buf, true)])
}

// coldConfig is a realistic library configuration: providers, routes, a price
// catalog with remote sources, caching — everything except an opted-into
// Postgres DSN.
func coldConfig(baseURL string, priceSources []string) *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{{
			Name: "fake", Type: config.TypePassthrough, BaseURL: baseURL, APIKey: "config-key",
		}},
		Routes:  []config.RouteConfig{{Model: "demo", Provider: "fake"}},
		Cache:   config.CacheConfig{Enabled: true, MaxEntries: 8},
		Pricing: config.PricingConfig{Sources: priceSources, SyncIntervalMinutes: 360},
	}
}

// Rule 1: New starts no goroutines. Background work is opt-in via Run/Start.
func TestConstructionStartsNoGoroutines(t *testing.T) {
	cfg := coldConfig("http://127.0.0.1:9/v1", []string{"http://127.0.0.1:9/prices.json"})

	before := settle(t)
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	after := settle(t)

	if after > before {
		t.Fatalf("gateway.New started %d goroutine(s) (%d -> %d). New must start NOTHING; "+
			"background work belongs in Start/Run.\n%s", after-before, before, after, goroutineDump())
	}

	// Positive control: the counter and the settle helper must be capable of
	// SEEING a goroutine, otherwise the assertion above is decoration. Start()
	// launches the pricing syncer, which is exactly what New must not do.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started := settle(t); started <= after {
		t.Fatalf("CONTROL FAILED: Start() did not raise the goroutine count (%d -> %d). "+
			"This measurement cannot detect a goroutine at all, so the assertion above proved nothing.",
			after, started)
	}
}

// recordingTransport counts every round trip and forwards to the real one.
type recordingTransport struct {
	next  http.RoundTripper
	count atomic.Int64
	urls  atomic.Value // []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.count.Add(1)
	prev, _ := r.urls.Load().([]string)
	r.urls.Store(append(append([]string{}, prev...), req.Method+" "+req.URL.String()))
	return r.next.RoundTrip(req)
}

func (r *recordingTransport) Count() int64 { return r.count.Load() }
func (r *recordingTransport) Seen() string {
	u, _ := r.urls.Load().([]string)
	return strings.Join(u, ", ")
}

// Rule 2 as a counted fact, not an inference from "no error was returned".
//
// The recorder is installed as http.DefaultTransport, which is what every
// client llmux builds resolves to (provider.NewHTTPClient and the pricing
// source client both leave Transport nil). Two positive controls prove it is
// actually on the path — without them a zero count would be indistinguishable
// from a recorder nothing uses.
func TestConstructionMakesZeroRoundTrips(t *testing.T) {
	// The premise: a stock llmux really does ship remote price feeds, so
	// "construction fetches nothing" is a claim with something to fetch. If the
	// defaults ever stop carrying sources, this guard is no longer testing what
	// it says it is.
	if got := len(config.Default().Pricing.Sources); got == 0 {
		t.Fatal("config.Default() ships no pricing sources — this guard's premise is gone; " +
			"it would pass vacuously")
	}

	var priceHits atomic.Int64
	prices := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		priceHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(prices.Close)

	up := newFakeUpstream(t, "answer")

	rec := &recordingTransport{next: http.DefaultTransport}
	orig := http.DefaultTransport
	http.DefaultTransport = rec
	t.Cleanup(func() { http.DefaultTransport = orig })

	cfg := coldConfig(up.URL, []string{prices.URL + "/prices.json"})
	if cfg.Postgres != "" {
		t.Fatal("coldConfig must carry no Postgres DSN — with one, New connects eagerly by design")
	}

	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	if n := rec.Count(); n != 0 {
		t.Fatalf("gateway.New made %d HTTP round trip(s): %s\nConstruction must open nothing without a "+
			"Postgres DSN.", n, rec.Seen())
	}
	if n := priceHits.Load(); n != 0 {
		t.Fatalf("gateway.New fetched the price feed %d time(s) — the syncer belongs to Start, not New", n)
	}

	// Control 1: the recorder sees the price feed the moment Start opts in.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for priceHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if priceHits.Load() == 0 {
		t.Fatal("CONTROL FAILED: Start() never fetched the price feed, so a construction-time fetch would " +
			"not have been counted either. This guard measured nothing.")
	}
	if rec.Count() == 0 {
		t.Fatal("CONTROL FAILED: the recording transport counted no round trip even after Start fetched " +
			"prices — it is not on the path llmux uses, so the zero above proved nothing.")
	}

	// Control 2: the provider adapter's client resolves to the recorder too, so
	// a construction-time dial to an UPSTREAM would have been counted as well.
	before := rec.Count()
	if _, err := gw.Chat(context.Background(), &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("ping")}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if rec.Count() <= before {
		t.Fatalf("CONTROL FAILED: a real upstream call did not go through the recorder (%d -> %d) — "+
			"provider dials are invisible to this guard", before, rec.Count())
	}
}

// The honest exception, asserted rather than glossed: a Postgres DSN IS an
// opted-into remote dependency that New connects to eagerly. This test exists
// so the exception is documented by a failing-if-changed fact, and so nobody
// "fixes" the guard above by pretending Postgres is lazy.
func TestPostgresDSNConnectsEagerlyAtConstruction(t *testing.T) {
	cfg := coldConfig("http://127.0.0.1:9/v1", nil)
	// Port 1 is reserved and never listening: a lazy pool would construct fine
	// and only fail on first use, so an error here IS the eager connect.
	cfg.Postgres = "postgres://llmux:llmux@127.0.0.1:1/llmux?sslmode=disable&connect_timeout=2"

	gw, err := gateway.New(cfg)
	if err == nil {
		_ = gw.Close()
		t.Fatal("gateway.New succeeded with an unreachable Postgres DSN — either the key store became lazy " +
			"(good: update this test AND the doc comment on New) or the DSN is being ignored (bad).")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("New failed, but not on Postgres: %v", err)
	}
	t.Logf("documented deviation confirmed — New dials Postgres eagerly: %v", err)
}

// The real environment property. NOT "New reads no environment" (false here:
// ResolveKey honours a config-named APIKeyEnv) but "New adopts no provider key
// the config never named" — which is the openrate-class bug this rule exists
// for.
func TestConstructionAdoptsNoUnnamedProviderKey(t *testing.T) {
	const sentinel = "sk-SENTINEL-must-never-be-adopted"
	for _, env := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "DEEPSEEK_API_KEY",
		"GROQ_API_KEY", "MISTRAL_API_KEY", "TOGETHER_API_KEY", "FIREWORKS_API_KEY",
		"XAI_API_KEY", "OPENROUTER_API_KEY", "COHERE_API_KEY", "LLMUX_LOCAL_API_KEY",
	} {
		t.Setenv(env, sentinel)
	}

	t.Run("an explicitly configured key wins over every env var", func(t *testing.T) {
		up := newFakeUpstream(t, "ok")
		cfg := coldConfig(up.URL, nil)
		gw, err := gateway.New(cfg)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}
		t.Cleanup(func() { _ = gw.Close() })

		if names := gw.Registry().Names(); len(names) != 1 || names[0] != "fake" {
			t.Fatalf("New registered %v — it invented providers from the environment; the config named one",
				names)
		}
		chat(t, gw)
		if auth := up.LastAuth(); auth != "Bearer config-key" {
			t.Fatalf("upstream saw Authorization %q, want %q", auth, "Bearer config-key")
		}
	})

	t.Run("a provider with no key configured sends no key at all", func(t *testing.T) {
		up := newFakeUpstream(t, "ok")
		cfg := coldConfig(up.URL, nil)
		cfg.Providers[0].APIKey = "" // names nothing: no api_key, no api_key_env
		gw, err := gateway.New(cfg)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}
		t.Cleanup(func() { _ = gw.Close() })

		chat(t, gw)
		if auth := up.LastAuth(); auth != "" {
			t.Fatalf("upstream saw Authorization %q on a provider with NO configured key — a key was picked "+
				"up from the environment behind the config's back", auth)
		}
	})
}

// The other half of the same truth: when the config NAMES an env var, New does
// read it. That is the documented, config-directed behaviour; assert it so the
// guard above cannot be "fixed" by breaking api_key_env.
func TestConstructionReadsOnlyTheEnvVarTheConfigNames(t *testing.T) {
	t.Setenv("LLMUX_EMBEDTEST_KEY", "named-env-key")
	t.Setenv("OPENAI_API_KEY", "sk-SENTINEL-must-never-be-adopted")

	up := newFakeUpstream(t, "ok")
	cfg := coldConfig(up.URL, nil)
	cfg.Providers[0].APIKey = ""
	cfg.Providers[0].APIKeyEnv = "LLMUX_EMBEDTEST_KEY"

	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	chat(t, gw)
	if auth := up.LastAuth(); auth != "Bearer named-env-key" {
		t.Fatalf("upstream saw Authorization %q, want the config-named env var's value %q",
			auth, "Bearer named-env-key")
	}
}

func chat(t *testing.T, gw *gateway.Gateway) {
	t.Helper()
	if _, err := gw.Chat(context.Background(), &openai.ChatCompletionRequest{
		Model:    "demo",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("ping")}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

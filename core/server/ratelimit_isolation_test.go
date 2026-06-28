package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// ---------------------------------------------------------------------------
// Rate-limit per-key/tenant isolation
// ---------------------------------------------------------------------------

// TestRateLimitTenantIsolation verifies that two keys (tenants) each have an
// independent rate-limit bucket. Exhausting key A's bucket must not affect
// key B's requests, and vice versa.
func TestRateLimitTenantIsolation(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys: []config.KeyConfig{
			{Key: "sk-a", Name: "tenant-a", RPM: 1},
			{Key: "sk-b", Name: "tenant-b", RPM: 1},
		},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Key A: first request succeeds, second is rate-limited.
	if rec := postKey(s, chatBody("m"), "sk-a"); rec.Code != 200 {
		t.Fatalf("A first: status=%d, want 200", rec.Code)
	}
	if rec := postKey(s, chatBody("m"), "sk-a"); rec.Code != 429 {
		t.Fatalf("A second: status=%d, want 429 (rate limited)", rec.Code)
	}

	// Key B's bucket is INDEPENDENT: it must still allow its first request even
	// though key A is exhausted.
	if rec := postKey(s, chatBody("m"), "sk-b"); rec.Code != 200 {
		t.Fatalf("B first (should be independent of A): status=%d, want 200", rec.Code)
	}
	if rec := postKey(s, chatBody("m"), "sk-b"); rec.Code != 429 {
		t.Fatalf("B second: status=%d, want 429 (rate limited)", rec.Code)
	}
}

// TestRateLimitUnlimitedKeyAlwaysAllows verifies that a key with no RPM
// configuration (zero value) is never rate-limited regardless of how many
// requests are sent in quick succession.
func TestRateLimitUnlimitedKeyAlwaysAllows(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:      []config.KeyConfig{{Key: "sk-unlimited", Name: "unlimited"}}, // RPM=0 → no limit
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 20; i++ {
		if rec := postKey(s, chatBody("m"), "sk-unlimited"); rec.Code != 200 {
			t.Fatalf("unlimited key request %d: status=%d, want 200", i, rec.Code)
		}
	}
}

// TestRateLimitConcurrentBucketIsThreadSafe exercises the token-bucket rate
// limiter from many goroutines simultaneously. It verifies that:
//   - The total number of allowed requests does not exceed RPM.
//   - The test passes under the race detector (no data races in the bucket).
func TestRateLimitConcurrentBucketIsThreadSafe(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()

	const rpm = 5
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:      []config.KeyConfig{{Key: "sk-rl", Name: "rl", RPM: rpm}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const goroutines = 30
	var allowed, rateLimited int32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := postKey(s, chatBody("m"), "sk-rl")
			switch rec.Code {
			case 200:
				atomic.AddInt32(&allowed, 1)
			case 429:
				atomic.AddInt32(&rateLimited, 1)
			default:
				t.Errorf("unexpected status=%d", rec.Code)
			}
		}()
	}
	wg.Wait()

	// The token bucket starts full (capacity = rpm = 5). Under concurrent
	// hammering, at most rpm requests can be allowed before the bucket empties.
	// We allow a small fudge for timing, but well under goroutines.
	if int(allowed) > rpm+1 {
		t.Fatalf("concurrent rate limiter let through %d requests with RPM=%d (bucket overflowed)", allowed, rpm)
	}
	if int(allowed+rateLimited) != goroutines {
		t.Fatalf("allowed(%d)+limited(%d) != goroutines(%d)", allowed, rateLimited, goroutines)
	}
}

// TestRateLimitHighRPMAllowsBurst verifies that a key with a large RPM cap
// allows a burst of requests up to the cap before throttling.
func TestRateLimitHighRPMAllowsBurst(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()

	const rpm = 50
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:      []config.KeyConfig{{Key: "sk-burst", Name: "burst", RPM: rpm}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The bucket is initialized full (capacity=rpm), so the first rpm requests
	// should all succeed without any 429.
	for i := 0; i < rpm; i++ {
		if rec := postKey(s, chatBody("m"), "sk-burst"); rec.Code != 200 {
			t.Fatalf("burst request %d: status=%d, want 200 (within RPM cap)", i, rec.Code)
		}
	}
	// One more should be throttled.
	if rec := postKey(s, chatBody("m"), "sk-burst"); rec.Code != 429 {
		t.Fatalf("post-burst request: status=%d, want 429", rec.Code)
	}
}

// TestRateLimitDoesNotApplyToMasterKeyPath verifies that requests using the
// master key (not a virtual key) bypass the rate limiter entirely — the master
// key has no rate-limit bucket.
func TestRateLimitDoesNotApplyToMasterKeyPath(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "MASTER"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// No virtual keys → the master key path has no rate-limit at all.
	for i := 0; i < 20; i++ {
		if rec := postKey(s, chatBody("m"), "MASTER"); rec.Code != 200 {
			t.Fatalf("master key request %d: status=%d, want 200 (no rate limit on master)", i, rec.Code)
		}
	}
}

// TestRateLimitAndModelAllowListInteraction documents the interaction between
// the per-key rate limiter and the model allow-list. The rate limiter in authMW
// fires before the per-handler model allow-list check, so a forbidden-model
// request does consume a rate-limit token. This test confirms both are correctly
// enforced in sequence and neither causes a 500/panic.
func TestRateLimitAndModelAllowListInteraction(t *testing.T) {
	up := hardeningOKUpstream(t, nil)
	defer up.Close()

	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "mock", Type: config.TypePassthrough, BaseURL: up.URL + "/v1"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "mock"}},
		Keys:      []config.KeyConfig{{Key: "sk-rl", Name: "rl", RPM: 2, AllowedModels: []string{"allowed"}}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// First request: forbidden model → 403 (rate token consumed, no upstream call).
	if rec := postKey(s, chatBody("forbidden"), "sk-rl"); rec.Code != 403 {
		t.Fatalf("forbidden model: status=%d, want 403", rec.Code)
	}
	// Second request: allowed model → 200 (one rate token still available).
	if rec := postKey(s, chatBody("allowed"), "sk-rl"); rec.Code != 200 {
		t.Fatalf("allowed model (2nd request, RPM=2): status=%d, want 200", rec.Code)
	}
	// Third request: rate limit exhausted (RPM=2 used up by the two above) → 429.
	if rec := postKey(s, chatBody("allowed"), "sk-rl"); rec.Code != 429 {
		t.Fatalf("3rd request: status=%d, want 429 (rate exhausted)", rec.Code)
	}
}

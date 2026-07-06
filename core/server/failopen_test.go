package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// These tests pin the fail-CLOSED behavior for a keyless gateway (no master key,
// no virtual keys, no cp identity). The historical bug: an empty LLMUX_MASTER_KEY
// left the server an OPEN proxy with /admin and /metrics reachable by anyone who
// could connect, and it bound a non-loopback address without complaint.
//
// Prior art mirrored here:
//   - sovereign.LocalityOf / isLoopbackHost classify loopback and fail closed on
//     an empty/unparseable target. remoteIsLoopback / addrIsLoopback below reuse
//     the same loopback classification and the same fail-closed default.
//   - byokGuardrailWarning (cmd/llmux) already recognized "no master key + TCP
//     bind" as dangerous; this fix upgrades that from a warning to a hard refusal
//     for the general open-proxy case and gates the disclosure endpoints.

// runWithCancelledCtx invokes Run with an already-cancelled context so it either
// returns the fail-closed refusal (before binding) or binds and immediately shuts
// down. Returns the error from Run (nil on a clean bind+shutdown).
func runWithCancelledCtx(s *Server) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return s.Run(ctx)
}

// TestFailOpen_KeylessNonLoopbackBindRefused: empty master key + non-loopback
// bind + no insecure opt-in → Run refuses to start (fails closed).
func TestFailOpen_KeylessNonLoopbackBindRefused(t *testing.T) {
	for _, addr := range []string{":4000", "0.0.0.0:4000", "192.0.2.10:4000", "[::]:4000"} {
		cfg := &config.Config{Server: config.ServerConfig{Addr: addr}}
		s, err := New(cfg)
		if err != nil {
			t.Fatalf("New(%q): %v", addr, err)
		}
		err = runWithCancelledCtx(s)
		if err == nil {
			t.Fatalf("addr %q: keyless non-loopback bind was accepted; want a refusal", addr)
		}
		if !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("addr %q: unexpected error %q, want a fail-closed refusal", addr, err)
		}
	}
}

// TestFailOpen_KeylessLoopbackBindAllowed: a keyless loopback bind stays allowed
// (dev ergonomics) — it binds and shuts down cleanly on the cancelled context.
func TestFailOpen_KeylessLoopbackBindAllowed(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		cfg := &config.Config{Server: config.ServerConfig{Addr: addr}}
		s, err := New(cfg)
		if err != nil {
			t.Fatalf("New(%q): %v", addr, err)
		}
		if err := runWithCancelledCtx(s); err != nil {
			t.Fatalf("addr %q: keyless loopback bind should be allowed, got %v", addr, err)
		}
	}
}

// TestFailOpen_InsecureOptInAllowsNonLoopback: the explicit insecure_keyless
// opt-in restores the non-loopback keyless bind (footgun override kept for
// operators who accept the exposure).
func TestFailOpen_InsecureOptInAllowsNonLoopback(t *testing.T) {
	// Wildcard bind (":0" = all interfaces, non-loopback) with the explicit
	// insecure opt-in: normally refused, but the opt-in permits it. The ephemeral
	// port keeps the listener bind itself successful.
	cfg := &config.Config{Server: config.ServerConfig{Addr: ":0", InsecureKeyless: true}}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runWithCancelledCtx(s); err != nil {
		t.Fatalf("insecure_keyless opt-in should permit a non-loopback keyless bind, got %v", err)
	}
}

// TestFailOpen_ConfiguredKeyBindsNonLoopback: a configured master key makes the
// non-loopback bind normal operation (no refusal).
func TestFailOpen_ConfiguredKeyBindsNonLoopback(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Addr: ":0", MasterKey: "MASTER"}}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runWithCancelledCtx(s); err != nil {
		t.Fatalf("configured master key should permit a non-loopback bind, got %v", err)
	}

	// Virtual keys (an identity wall) also count as "not keyless".
	cfg2 := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Keys:   []config.KeyConfig{{Key: "sk-a", Name: "team-a", BudgetUSD: 10}},
	}
	s2, err := New(cfg2)
	if err != nil {
		t.Fatalf("New(keys): %v", err)
	}
	if err := runWithCancelledCtx(s2); err != nil {
		t.Fatalf("virtual keys should permit a non-loopback bind, got %v", err)
	}
}

// getFrom issues a GET with an explicit RemoteAddr so the caller's locality can
// be controlled (loopback vs network).
func getFrom(s *Server, path, remoteAddr, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = remoteAddr
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestFailOpen_KeylessAdminMetricsClosedToNonLoopback: on a keyless box, /admin
// and /metrics must NOT be open to a non-loopback caller (the core disclosure
// leak), while a loopback caller keeps dev access.
func TestFailOpen_KeylessAdminMetricsClosedToNonLoopback(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: "127.0.0.1:0"},
		Providers: []config.ProviderConfig{{Name: "secretprov", Type: config.TypePassthrough, BaseURL: "https://internal.example/v1"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const netAddr = "192.0.2.10:5555" // TEST-NET-1: non-loopback
	const loopAddr = "127.0.0.1:5555"

	for _, path := range []string{"/admin/keys", "/admin/usage", "/metrics"} {
		// Non-loopback caller on a keyless box → 401 (fails closed, not open).
		if rec := getFrom(s, path, netAddr, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s from network on keyless box: status=%d, want 401 (must not be open)", path, rec.Code)
		}
		// Loopback caller keeps keyless dev access → 200.
		if rec := getFrom(s, path, loopAddr, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s from loopback on keyless box: status=%d, want 200 (dev ergonomics)", path, rec.Code)
		}
	}

	// The /health provider-topology disclosure must also stay closed to a
	// non-loopback caller on a keyless box (no key list to authenticate with).
	rec := getFrom(s, "/health", netAddr, "")
	if strings.Contains(rec.Body.String(), "providers") || strings.Contains(rec.Body.String(), "secretprov") {
		t.Fatalf("/health leaked provider topology to a non-loopback caller on a keyless box: %s", rec.Body.String())
	}
	// But a loopback caller may see it (local operator).
	rec = getFrom(s, "/health", loopAddr, "")
	if !strings.Contains(rec.Body.String(), "secretprov") {
		t.Fatalf("/health should disclose topology to a loopback caller on a keyless box: %s", rec.Body.String())
	}
}

// TestFailOpen_ConfiguredKeyAdminMetricsNormal: with a master key, /admin and
// /metrics work normally for the key holder regardless of caller address, and a
// missing/wrong key is 401 (unchanged behavior, from any address).
func TestFailOpen_ConfiguredKeyAdminMetricsNormal(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: "127.0.0.1:0", MasterKey: "MASTER"},
		Keys:   []config.KeyConfig{{Key: "sk-virtual", Name: "tenant", BudgetUSD: 100}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const netAddr = "192.0.2.10:5555"
	for _, path := range []string{"/admin/keys", "/admin/usage", "/metrics"} {
		// Master key from the network → 200 (address is irrelevant when a key gates).
		if rec := getFrom(s, path, netAddr, "MASTER"); rec.Code != http.StatusOK {
			t.Fatalf("%s master-key from network: status=%d, want 200", path, rec.Code)
		}
		// No key → 401 even from loopback (a configured key is mandatory).
		if rec := getFrom(s, path, "127.0.0.1:1", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s no-key with configured master: status=%d, want 401", path, rec.Code)
		}
		// Virtual key → 401 (privileged endpoints never accept virtual keys).
		if rec := getFrom(s, path, netAddr, "sk-virtual"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s virtual-key: status=%d, want 401", path, rec.Code)
		}
	}
}

// TestFailOpen_AddrIsLoopback pins the address classifier: wildcard/omitted host
// is treated as network-reachable (fail closed), explicit loopback is loopback.
func TestFailOpen_AddrIsLoopback(t *testing.T) {
	loopback := []string{"127.0.0.1:4000", "localhost:4000", "[::1]:4000", "127.0.0.1", "::1"}
	network := []string{":4000", "0.0.0.0:4000", "[::]:4000", "192.0.2.10:4000", "example.com:4000", ""}
	for _, a := range loopback {
		if !addrIsLoopback(a) {
			t.Errorf("addrIsLoopback(%q)=false, want true", a)
		}
	}
	for _, a := range network {
		if addrIsLoopback(a) {
			t.Errorf("addrIsLoopback(%q)=true, want false (fail closed)", a)
		}
	}
}

// TestFailOpen_RemoteIsLoopback pins the caller classifier used to gate keyless
// disclosure: unparseable/empty-host peers fail closed (non-loopback), a unix
// socket (empty RemoteAddr) is local.
func TestFailOpen_RemoteIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:1234", true},
		{"", true}, // unix socket: no network peer, owner-only by socket perms
		{"192.0.2.10:1234", false},
		{"10.0.0.5:1234", false}, // private LAN is NOT loopback
		{"garbage", false},       // unparseable → fail closed
		{"notanip:80", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = c.addr
		if got := remoteIsLoopback(req); got != c.want {
			t.Errorf("remoteIsLoopback(%q)=%v, want %v", c.addr, got, c.want)
		}
	}
}

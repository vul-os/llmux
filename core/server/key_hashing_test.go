package server

import (
	"context"
	"strings"
	"testing"

	"github.com/llmux/llmux/core/keys"
)

// TestCacheScopeHashesToken verifies that cacheScope returns the SHA-256 hash
// of the virtual key's raw token, never the raw token itself. This is the
// at-rest secret protection for the Redis cache keyspace: a SCAN/MONITOR of
// Redis must never yield a live bearer credential.
func TestCacheScopeHashesToken(t *testing.T) {
	const rawToken = "sk-live-secret"

	k := &keys.Key{Key: rawToken, Name: "test"}
	ctx := withKey(context.Background(), k)

	scope := cacheScope(ctx)

	// Must equal sha256(rawToken) hex — identical to keys.HashToken.
	want := keys.HashToken(rawToken)
	if scope != want {
		t.Fatalf("cacheScope = %q, want hash %q", scope, want)
	}
	// Must NOT contain the raw token.
	if strings.Contains(scope, rawToken) {
		t.Fatalf("cacheScope contains raw token: %q", scope)
	}
	// Must be 64 hex chars (256-bit SHA-256).
	if len(scope) != 64 {
		t.Fatalf("cacheScope length = %d, want 64", len(scope))
	}
}

// TestCacheScopeAccountFallback verifies that cacheScope falls back to the
// account id (not a key hash) when no virtual key is in context.
func TestCacheScopeAccountFallback(t *testing.T) {
	ctx := withAccount(context.Background(), "acct-123")
	if scope := cacheScope(ctx); scope != "acct-123" {
		t.Fatalf("cacheScope (account) = %q, want %q", scope, "acct-123")
	}
}

// TestCacheScopeUnauthenticated verifies that cacheScope returns "" for an
// unauthenticated (open/local-mode) request.
func TestCacheScopeUnauthenticated(t *testing.T) {
	if scope := cacheScope(context.Background()); scope != "" {
		t.Fatalf("cacheScope (unauth) = %q, want %q", scope, "")
	}
}

package keys

import (
	"strings"
	"testing"
)

// TestHashTokenDeterministic verifies that HashToken returns a stable
// hex-encoded SHA-256 and that the raw token never appears in the output.
func TestHashTokenDeterministic(t *testing.T) {
	const token = "sk-test-secret-bearer"
	// sha256("sk-test-secret-bearer") pre-computed for regression detection.
	const wantHex = "98c77c70cc70af9f1ba3f9556685c635205792cee997a4ce5d87c171112325cf"

	got := HashToken(token)
	if got != wantHex {
		t.Fatalf("HashToken(%q) = %q, want %q", token, got, wantHex)
	}
	// Hash must be 64 hex chars (256 bits).
	if len(got) != 64 {
		t.Fatalf("HashToken length = %d, want 64", len(got))
	}
	// The raw token must not appear in the hash.
	if strings.Contains(got, token) {
		t.Fatalf("raw token appears in hash output: %q", got)
	}
	// Idempotent: same input → same output.
	if HashToken(token) != got {
		t.Fatal("HashToken is not deterministic")
	}
	// Different inputs → different hashes.
	if HashToken(token+"x") == got {
		t.Fatal("HashToken collision on different inputs")
	}
}

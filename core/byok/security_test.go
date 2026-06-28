package byok

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Encryption-at-rest guarantees
// ---------------------------------------------------------------------------

// TestBYOKCiphertextNeverContainsPlaintext verifies that the raw on-disk or
// in-memory bytes of a sealed BYOK key contain no substring of the plaintext
// secret. This guards against an accidental truncation or encoding bug that
// could leave part of the key in plaintext.
func TestBYOKCiphertextNeverContainsPlaintext(t *testing.T) {
	c := testCrypter(t)
	secrets := []string{
		"sk-1234567890abcdef",
		"Bearer token with spaces",
		"AIzaSyABCDEF01234567",
		"xoxb-slack-token",
	}
	for _, secret := range secrets {
		blob, err := c.seal(secret)
		if err != nil {
			t.Fatalf("seal(%q): %v", secret, err)
		}
		if strings.Contains(blob, secret) {
			t.Errorf("seal(%q): ciphertext contains plaintext key", secret)
		}
		// Also confirm the raw base64 blob doesn't contain any 8-char run from
		// the secret — a partial-leak guard.
		for i := 0; i+8 <= len(secret); i++ {
			chunk := secret[i : i+8]
			if strings.Contains(blob, chunk) {
				t.Errorf("seal(%q): ciphertext contains plaintext fragment %q at offset %d", secret, chunk, i)
			}
		}
	}
}

// TestBYOKFileStoreWrongKEKReturnsEmpty verifies that opening a FileStore with
// a different KEK from the one used to write the data silently returns ("", false)
// from Get — no panic, no plaintext, no crash.
func TestBYOKFileStoreWrongKEKReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "byok.json")

	kek1 := make([]byte, 32)
	for i := range kek1 {
		kek1[i] = 0x01
	}
	kek2 := make([]byte, 32)
	for i := range kek2 {
		kek2[i] = 0x02
	}

	c1, _ := NewCrypter(kek1)
	fs1, err := NewFileStore(c1, path)
	if err != nil {
		t.Fatalf("NewFileStore(kek1): %v", err)
	}
	if err := fs1.Set("acct", "openai", "sk-secret-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Open the same file with a different KEK — decryption must fail gracefully.
	c2, _ := NewCrypter(kek2)
	fs2, err := NewFileStore(c2, path)
	if err != nil {
		t.Fatalf("NewFileStore(kek2): %v", err)
	}
	got, ok := fs2.Get("acct", "openai")
	if ok {
		t.Fatalf("wrong-KEK Get returned ok=true with value %q", got)
	}
	if got != "" {
		t.Fatalf("wrong-KEK Get returned non-empty value %q", got)
	}
	// Plaintext must not appear in the returned value.
	if strings.Contains(got, "sk-secret") {
		t.Fatalf("wrong-KEK Get leaked plaintext: %q", got)
	}
}

// TestBYOKFileStoreAtomicRewrite verifies that the file store writes via a
// temp-file + rename pattern: the file content must be valid JSON at all times.
// We simulate this by checking that the store file exists and has no companion
// .tmp file left behind after a successful write.
func TestBYOKFileStoreAtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "byok.json")
	c := testCrypter(t)

	fs, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := fs.Set("acct", fmt.Sprintf("prov%d", i), "key"); err != nil {
			t.Fatalf("Set iteration %d: %v", i, err)
		}
	}

	// No .tmp file should linger after a successful write.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp file left behind after write: %v", err)
	}
	// The store file itself must be readable.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("store file missing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cross-account isolation
// ---------------------------------------------------------------------------

// TestBYOKCrossAccountIsolation verifies that storing a key for account A does
// not make it accessible to account B — even when the account identifiers are
// close strings (e.g. prefix of each other).
func TestBYOKCrossAccountIsolation(t *testing.T) {
	c := testCrypter(t)
	s := NewMemStore(c)

	accounts := []string{"alice", "alice2", "bob", "alice-admin"}
	for _, acct := range accounts {
		if err := s.Set(acct, "openai", "key-for-"+acct); err != nil {
			t.Fatalf("Set(%s): %v", acct, err)
		}
	}

	for _, acct := range accounts {
		k, ok := s.Get(acct, "openai")
		if !ok {
			t.Fatalf("Get(%s): expected key, got nothing", acct)
		}
		if k != "key-for-"+acct {
			t.Fatalf("Get(%s) = %q, want %q (cross-account leak)", acct, k, "key-for-"+acct)
		}
	}

	// Specifically: "alice" cannot read "alice2"'s key and vice versa.
	aliceKey, _ := s.Get("alice", "openai")
	alice2Key, _ := s.Get("alice2", "openai")
	if aliceKey == alice2Key {
		t.Fatal("alice and alice2 returned the same key (isolation failure)")
	}
}

// TestBYOKCrossAccountClearDoesNotAffectOther verifies that clearing a BYOK
// key for one account does not remove or affect another account's key for the
// same provider.
func TestBYOKCrossAccountClearDoesNotAffectOther(t *testing.T) {
	c := testCrypter(t)
	s := NewMemStore(c)

	_ = s.Set("alice", "openai", "alice-key")
	_ = s.Set("bob", "openai", "bob-key")

	// Clear alice's key.
	if err := s.Clear("alice", "openai"); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	// Alice's key gone.
	if _, ok := s.Get("alice", "openai"); ok {
		t.Fatal("alice's key still present after Clear")
	}
	// Bob's key unaffected.
	if k, ok := s.Get("bob", "openai"); !ok || k != "bob-key" {
		t.Fatalf("bob's key affected by alice's Clear: ok=%v k=%q", ok, k)
	}
}

// ---------------------------------------------------------------------------
// Concurrent access safety (race detector)
// ---------------------------------------------------------------------------

// TestBYOKMemStoreConcurrentAccessRace exercises concurrent Set/Get/Clear
// from many goroutines. The test is intended to be run with -race to detect
// any data race in the MemStore locking.
func TestBYOKMemStoreConcurrentAccessRace(t *testing.T) {
	c := testCrypter(t)
	s := NewMemStore(c)

	const goroutines = 20
	const ops = 50
	accounts := []string{"alice", "bob", "carol", "dave"}
	providers := []string{"openai", "anthropic", "gemini"}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			acct := accounts[id%len(accounts)]
			prov := providers[id%len(providers)]
			for i := 0; i < ops; i++ {
				switch i % 3 {
				case 0:
					_ = s.Set(acct, prov, fmt.Sprintf("key-%d-%d", id, i))
				case 1:
					_, _ = s.Get(acct, prov)
				case 2:
					_ = s.Clear(acct, prov)
				}
				_ = s.Providers(acct)
			}
		}(g)
	}
	wg.Wait()
}

// TestBYOKFileStoreConcurrentWritesRace exercises concurrent writes to the
// FileStore to confirm the flush-locked path is data-race-free.
func TestBYOKFileStoreConcurrentWritesRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "byok.json")
	c := testCrypter(t)

	fs, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			acct := fmt.Sprintf("acct-%d", id)
			if err := fs.Set(acct, "openai", fmt.Sprintf("key-%d", id)); err != nil {
				t.Errorf("Set goroutine %d: %v", id, err)
			}
		}(g)
	}
	wg.Wait()

	// All writes must have persisted: re-open and verify each key.
	fs2, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for g := 0; g < goroutines; g++ {
		acct := fmt.Sprintf("acct-%d", g)
		k, ok := fs2.Get(acct, "openai")
		if !ok {
			t.Errorf("acct %s: key missing after concurrent writes", acct)
		}
		if k != fmt.Sprintf("key-%d", g) {
			t.Errorf("acct %s: key=%q, want key-%d", acct, k, g)
		}
	}
}

// ---------------------------------------------------------------------------
// Key material never in error returns
// ---------------------------------------------------------------------------

// TestBYOKGetAfterClearNeverLeaks verifies that a Get after a Clear of the
// same key returns ("", false) — never the old key value.
func TestBYOKGetAfterClearNeverLeaks(t *testing.T) {
	c := testCrypter(t)
	s := NewMemStore(c)

	secret := "sk-very-secret-provider-key-1234"
	_ = s.Set("acct", "openai", secret)
	_ = s.Clear("acct", "openai")

	got, ok := s.Get("acct", "openai")
	if ok || got != "" {
		t.Fatalf("Get after Clear: ok=%v value=%q (must be empty)", ok, got)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("Get after Clear leaked secret: %q", got)
	}
}

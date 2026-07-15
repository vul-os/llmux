package byok

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCrypter(t *testing.T) *Crypter {
	t.Helper()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	c, err := NewCrypter(kek)
	if err != nil {
		t.Fatalf("NewCrypter: %v", err)
	}
	return c
}

func TestCrypterRoundTrip(t *testing.T) {
	c := testCrypter(t)
	secret := "sk-super-secret-provider-key"
	blob, err := c.seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Encrypted at rest: the ciphertext must not contain the plaintext.
	if strings.Contains(blob, secret) {
		t.Fatalf("ciphertext leaks plaintext: %q", blob)
	}
	got, err := c.open(blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != secret {
		t.Fatalf("roundtrip mismatch: got %q want %q", got, secret)
	}
	// A second seal must differ (random nonce) yet decrypt to the same value.
	blob2, _ := c.seal(secret)
	if blob2 == blob {
		t.Fatal("two seals produced identical ciphertext (nonce reuse?)")
	}
}

func TestCrypterWrongKEKFails(t *testing.T) {
	c := testCrypter(t)
	blob, _ := c.seal("hello")
	other := make([]byte, 32) // all zeros — different key
	c2, _ := NewCrypter(other)
	if _, err := c2.open(blob); err == nil {
		t.Fatal("decrypt with wrong KEK should fail")
	}
}

func TestNewCrypterBadKeyLen(t *testing.T) {
	if _, err := NewCrypter([]byte("short")); err == nil {
		t.Fatal("expected error for non-32-byte KEK")
	}
}

func TestParseKEK(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	cases := []string{
		hex.EncodeToString(raw),
		base64.StdEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
		string(raw),
	}
	for i, s := range cases {
		got, err := ParseKEK(s)
		if err != nil {
			t.Fatalf("case %d: ParseKEK: %v", i, err)
		}
		if len(got) != 32 {
			t.Fatalf("case %d: len=%d", i, len(got))
		}
	}
	if _, err := ParseKEK(""); err == nil {
		t.Fatal("empty KEK should error")
	}
	if _, err := ParseKEK("too-short"); err == nil {
		t.Fatal("invalid KEK should error")
	}
}

func TestMemStoreCRUD(t *testing.T) {
	s := NewMemStore(testCrypter(t))
	if _, ok := s.Get("a", "openai"); ok {
		t.Fatal("empty store should miss")
	}
	if err := s.Set("a", "openai", "k1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.Set("a", "anthropic", "k2"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if k, ok := s.Get("a", "openai"); !ok || k != "k1" {
		t.Fatalf("get openai = %q,%v", k, ok)
	}
	provs := s.Providers("a")
	if len(provs) != 2 || provs[0] != "anthropic" || provs[1] != "openai" {
		t.Fatalf("providers sorted = %v", provs)
	}
	if err := s.Clear("a", "openai"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := s.Get("a", "openai"); ok {
		t.Fatal("cleared key should miss")
	}
	// Account isolation.
	if _, ok := s.Get("b", "anthropic"); ok {
		t.Fatal("other account must not see keys")
	}
}

func TestStoreSetValidation(t *testing.T) {
	s := NewMemStore(testCrypter(t))
	if err := s.Set("", "openai", "k"); err == nil {
		t.Fatal("empty account should error")
	}
	if err := s.Set("a", "", "k"); err == nil {
		t.Fatal("empty provider should error")
	}
	if err := s.Set("a", "openai", ""); err == nil {
		t.Fatal("empty key should error (use Clear)")
	}
}

func TestFileStorePersistsEncrypted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "byok.json")
	c := testCrypter(t)

	fs, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Set("acct", "openai", "sk-secret-123"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// On-disk bytes must be ciphertext, never the plaintext key.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(raw), "sk-secret-123") {
		t.Fatalf("on-disk store leaks plaintext: %s", raw)
	}

	// A fresh store over the same file + KEK must decrypt the value.
	fs2, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if k, ok := fs2.Get("acct", "openai"); !ok || k != "sk-secret-123" {
		t.Fatalf("reopened get = %q,%v", k, ok)
	}
}

// TestFileStoreClearAndProvidersPersist exercises the FileStore lifecycle beyond
// Set/Get: Providers must list only the account's live providers (sorted), and a
// Clear must persist across a reopen (a cleared key must never reappear from the
// on-disk file).
func TestFileStoreClearAndProvidersPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "byok.json")
	c := testCrypter(t)

	fs, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Set("acct", "openai", "k-openai"); err != nil {
		t.Fatalf("set openai: %v", err)
	}
	if err := fs.Set("acct", "anthropic", "k-anthropic"); err != nil {
		t.Fatalf("set anthropic: %v", err)
	}

	if provs := fs.Providers("acct"); len(provs) != 2 || provs[0] != "anthropic" || provs[1] != "openai" {
		t.Fatalf("Providers sorted = %v, want [anthropic openai]", provs)
	}
	// An account with no keys lists nothing.
	if provs := fs.Providers("nobody"); provs != nil {
		t.Fatalf("empty account Providers = %v, want nil", provs)
	}

	// Clear one provider and confirm it is gone from both the live view and disk.
	if err := fs.Clear("acct", "openai"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if provs := fs.Providers("acct"); len(provs) != 1 || provs[0] != "anthropic" {
		t.Fatalf("after clear Providers = %v, want [anthropic]", provs)
	}

	// Reopen: the clear must have persisted, and the surviving key must decrypt.
	fs2, err := NewFileStore(c, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := fs2.Get("acct", "openai"); ok {
		t.Fatal("cleared provider reappeared after reopen")
	}
	if k, ok := fs2.Get("acct", "anthropic"); !ok || k != "k-anthropic" {
		t.Fatalf("surviving key after reopen = %q,%v", k, ok)
	}
	// Clearing the last provider drops the account entry entirely.
	if err := fs2.Clear("acct", "anthropic"); err != nil {
		t.Fatalf("clear last: %v", err)
	}
	if provs := fs2.Providers("acct"); provs != nil {
		t.Fatalf("account with all keys cleared should list nil, got %v", provs)
	}
}

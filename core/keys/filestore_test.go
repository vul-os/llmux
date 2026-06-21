package keys

import (
	"path/filepath"
	"testing"

	"github.com/llmux/llmux/core/config"
)

func TestFileStorePersistsSpend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	cfgs := []config.KeyConfig{{Key: "sk", Name: "team", BudgetUSD: 10}}

	fs1, err := NewFileStore(cfgs, path)
	if err != nil {
		t.Fatal(err)
	}
	fs1.AddSpend("sk", 3.5)
	if err := fs1.Flush(); err != nil {
		t.Fatal(err)
	}

	// A fresh store from the same path must see the persisted spend.
	fs2, err := NewFileStore(cfgs, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fs2.Spend("sk"); got != 3.5 {
		t.Fatalf("persisted spend=%v, want 3.5", got)
	}
}

func TestFileStoreBudgetPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	cfgs := []config.KeyConfig{{Key: "sk", BudgetUSD: 5}}

	fs1, _ := NewFileStore(cfgs, path)
	fs1.AddSpend("sk", 6)
	fs1.Flush()

	fs2, _ := NewFileStore(cfgs, path)
	if !fs2.OverBudget("sk") {
		t.Fatal("over-budget state should persist across restarts")
	}
}

func TestFileStoreFlushOnlyWhenDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	fs, _ := NewFileStore([]config.KeyConfig{{Key: "sk"}}, path)
	// No spend yet -> flush is a no-op and writes nothing.
	if err := fs.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Glob(path); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreImplementsStore(t *testing.T) {
	var _ Store = (*FileStore)(nil)
}

package cache

import (
	"testing"
	"time"
)

// resp builds a cache Entry whose body encodes the given id (so tests can assert
// which entry came back).
func resp(id string) *Entry {
	return &Entry{Body: []byte(id)}
}

func TestLRUGetSet(t *testing.T) {
	c := NewLRU(10, 0)
	if _, ok := c.Get("a"); ok {
		t.Fatal("empty cache should miss")
	}
	c.Set("a", resp("1"))
	got, ok := c.Get("a")
	if !ok || string(got.Body) != "1" {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestLRUEviction(t *testing.T) {
	c := NewLRU(2, 0)
	c.Set("a", resp("a"))
	c.Set("b", resp("b"))
	c.Get("a")            // make 'a' most-recently-used
	c.Set("c", resp("c")) // should evict 'b' (LRU)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should remain")
	}
	if c.Len() != 2 {
		t.Fatalf("len=%d", c.Len())
	}
}

func TestLRUTTL(t *testing.T) {
	c := NewLRU(10, 50*time.Millisecond)
	base := time.Now()
	c.now = func() time.Time { return base }
	c.Set("a", resp("a"))
	c.now = func() time.Time { return base.Add(100 * time.Millisecond) }
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestKeyForStable(t *testing.T) {
	if KeyFor([]byte("x")) != KeyFor([]byte("x")) {
		t.Fatal("same input must hash equally")
	}
	if KeyFor([]byte("x")) == KeyFor([]byte("y")) {
		t.Fatal("different input must differ")
	}
}

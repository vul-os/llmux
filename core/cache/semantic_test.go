package cache

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// fakeEmbedder maps known phrases to fixed vectors and falls back to a
// deterministic hash-based vector for anything else. If errOn matches the text,
// it returns an error to exercise graceful-degradation paths.
type fakeEmbedder struct {
	fixed map[string][]float64
	errOn string
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	if f.errOn != "" && text == f.errOn {
		return nil, errors.New("embed failed")
	}
	if v, ok := f.fixed[text]; ok {
		return v, nil
	}
	return hashVec(text), nil
}

// hashVec produces a small deterministic vector from text. Different text yields
// different (and generally near-orthogonal) vectors.
func hashVec(text string) []float64 {
	v := make([]float64, 8)
	var h uint32 = 2166136261
	for i := 0; i < len(text); i++ {
		h ^= uint32(text[i])
		h *= 16777619
		v[int(h)%len(v)] += 1
	}
	return v
}

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float64
		want float64
	}{
		{[]float64{1, 0}, []float64{1, 0}, 1},
		{[]float64{1, 0}, []float64{0, 1}, 0},
		{[]float64{1, 0}, []float64{-1, 0}, -1},
		{[]float64{1, 1}, []float64{2, 2}, 1},
		{[]float64{0, 0}, []float64{1, 1}, 0},    // zero vector
		{[]float64{1, 2, 3}, []float64{1, 2}, 0}, // mismatched length
		{nil, nil, 0},
	}
	for i, c := range cases {
		if got := cosine(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("case %d: cosine=%v want %v", i, got, c.want)
		}
	}
}

func TestSemanticExactHit(t *testing.T) {
	emb := &fakeEmbedder{fixed: map[string][]float64{
		"hello world": {1, 0, 0},
	}}
	c := NewSemanticCache(emb, 0.95, 10, 0)
	c.Set("hello world", resp("r1"))
	got, ok := c.Get("hello world")
	if !ok || string(got.Body) != "r1" {
		t.Fatalf("exact text should hit, got %v %v", got, ok)
	}
}

func TestSemanticNearDuplicateHit(t *testing.T) {
	emb := &fakeEmbedder{fixed: map[string][]float64{
		"what is the capital of france": {1, 0, 0},
		"what's the capital of france?": {0.99, 0.1, 0}, // cosine ~0.995
	}}
	c := NewSemanticCache(emb, 0.95, 10, 0)
	c.Set("what is the capital of france", resp("paris"))
	got, ok := c.Get("what's the capital of france?")
	if !ok || string(got.Body) != "paris" {
		t.Fatalf("near-duplicate should hit, got %v %v", got, ok)
	}
}

func TestSemanticBelowThresholdMiss(t *testing.T) {
	emb := &fakeEmbedder{fixed: map[string][]float64{
		"alpha": {1, 0, 0},
		"beta":  {0, 1, 0}, // orthogonal, cosine 0
	}}
	c := NewSemanticCache(emb, 0.95, 10, 0)
	c.Set("alpha", resp("a"))
	if _, ok := c.Get("beta"); ok {
		t.Fatal("dissimilar query should miss")
	}
}

func TestSemanticTTLExpiry(t *testing.T) {
	emb := &fakeEmbedder{fixed: map[string][]float64{"q": {1, 0}}}
	c := NewSemanticCache(emb, 0.95, 10, 50*time.Millisecond)
	base := time.Now()
	c.now = func() time.Time { return base }
	c.Set("q", resp("r"))
	if _, ok := c.Get("q"); !ok {
		t.Fatal("should hit before expiry")
	}
	c.now = func() time.Time { return base.Add(100 * time.Millisecond) }
	if _, ok := c.Get("q"); ok {
		t.Fatal("should miss after expiry")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry should be pruned, len=%d", c.Len())
	}
}

func TestSemanticCapacityEviction(t *testing.T) {
	emb := &fakeEmbedder{fixed: map[string][]float64{
		"a": {1, 0, 0},
		"b": {0, 1, 0},
		"c": {0, 0, 1},
	}}
	c := NewSemanticCache(emb, 0.95, 2, 0)
	c.Set("a", resp("a"))
	c.Set("b", resp("b"))
	c.Set("c", resp("c")) // evicts oldest "a"
	if c.Len() != 2 {
		t.Fatalf("len=%d, want 2", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted (FIFO)")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should remain")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should remain")
	}
}

func TestSemanticEmbedderErrorGraceful(t *testing.T) {
	emb := &fakeEmbedder{
		fixed: map[string][]float64{"good": {1, 0}},
		errOn: "bad",
	}
	c := NewSemanticCache(emb, 0.95, 10, 0)

	// Set with failing embed is a no-op.
	c.Set("bad", resp("x"))
	if c.Len() != 0 {
		t.Fatalf("failed Set should store nothing, len=%d", c.Len())
	}

	// Get with failing embed is a miss, no panic.
	if _, ok := c.Get("bad"); ok {
		t.Fatal("failed Get should miss")
	}

	// Healthy path still works.
	c.Set("good", resp("g"))
	if got, ok := c.Get("good"); !ok || string(got.Body) != "g" {
		t.Fatalf("healthy path broken, got %v %v", got, ok)
	}
}

func TestSemanticDefaultMax(t *testing.T) {
	c := NewSemanticCache(&fakeEmbedder{}, 0.95, 0, 0)
	if c.max != 10000 {
		t.Fatalf("default max=%d, want 10000", c.max)
	}
}

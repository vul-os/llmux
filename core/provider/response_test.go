package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWithHeaderSinkAndSinkFrom(t *testing.T) {
	ctx, s := WithHeaderSink(context.Background())
	if s == nil {
		t.Fatal("WithHeaderSink returned nil sink")
	}
	if got := SinkFrom(ctx); got != s {
		t.Fatal("SinkFrom should return the attached sink")
	}
}

func TestSinkFromMissing(t *testing.T) {
	if got := SinkFrom(context.Background()); got != nil {
		t.Fatalf("SinkFrom on bare ctx = %v, want nil", got)
	}
}

func TestCaptureAllowlist(t *testing.T) {
	_, s := WithHeaderSink(context.Background())
	src := http.Header{
		"X-Ratelimit-Limit-Requests":     {"5000"},
		"X-Ratelimit-Remaining-Requests": {"4999"},
		"Retry-After":                    {"30"},
		"Authorization":                  {"Bearer secret"},
		"Content-Type":                   {"application/json"},
		"Set-Cookie":                     {"session=abc"},
	}
	s.Capture(src)

	h := s.Header()
	if h.Get("X-Ratelimit-Limit-Requests") != "5000" {
		t.Errorf("missing ratelimit-limit header: %v", h)
	}
	if h.Get("X-Ratelimit-Remaining-Requests") != "4999" {
		t.Errorf("missing ratelimit-remaining header: %v", h)
	}
	if h.Get("Retry-After") != "30" {
		t.Errorf("missing retry-after header: %v", h)
	}
	// Disallowed headers must be dropped.
	for _, k := range []string{"Authorization", "Content-Type", "Set-Cookie"} {
		if h.Get(k) != "" {
			t.Errorf("header %q should have been dropped, got %q", k, h.Get(k))
		}
	}
}

func TestHeaderReturnsCopy(t *testing.T) {
	_, s := WithHeaderSink(context.Background())
	s.Capture(http.Header{"Retry-After": {"10"}})

	h := s.Header()
	h.Set("Retry-After", "999")
	h.Set("X-Injected", "evil")

	// Mutating the returned map must not affect the sink.
	again := s.Header()
	if again.Get("Retry-After") != "10" {
		t.Errorf("sink was mutated via returned map: %q", again.Get("Retry-After"))
	}
	if again.Get("X-Injected") != "" {
		t.Errorf("sink gained injected header: %q", again.Get("X-Injected"))
	}
}

func TestNilSinkSafety(t *testing.T) {
	var s *HeaderSink
	// Capture on nil receiver must be a no-op (no panic).
	s.Capture(http.Header{"Retry-After": {"1"}})
	// Header on nil receiver returns nil.
	if got := s.Header(); got != nil {
		t.Fatalf("nil sink Header() = %v, want nil", got)
	}
}

func TestBodyUnlimited(t *testing.T) {
	orig := MaxResponseBytes
	defer func() { MaxResponseBytes = orig }()
	MaxResponseBytes = 0

	payload := strings.Repeat("x", 1000)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(payload))}

	got, err := io.ReadAll(Body(resp))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("read %d bytes, want %d (full body)", len(got), len(payload))
	}
}

func TestBodyTruncated(t *testing.T) {
	orig := MaxResponseBytes
	defer func() { MaxResponseBytes = orig }()
	MaxResponseBytes = 16

	payload := strings.Repeat("y", 1000)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(payload))}

	got, err := io.ReadAll(Body(resp))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != MaxResponseBytes {
		t.Fatalf("read %d bytes, want %d (capped)", len(got), MaxResponseBytes)
	}
}

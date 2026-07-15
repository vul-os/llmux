package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// TestJSONLUsageLoggerRoundTrip is the billing-ledger money boundary: the JSONL
// logger is the durable usage record a billing sink replays, so every accounting
// field (tokens, cost, model, account, idempotency id) must survive a
// serialize/parse round-trip byte-for-byte.
func TestJSONLUsageLoggerRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := NewJSONLUsageLogger(&buf)
	rec := UsageRecord{
		ID:         "usage-abc123",
		Time:       "2026-07-15T00:00:00Z",
		KeyName:    "team-alpha",
		AccountID:  "acct_9",
		Model:      "openai/gpt-4o",
		Stream:     true,
		Prompt:     1200,
		Completion: 800,
		Total:      2000,
		CostUSD:    0.0125,
	}
	l.Log(rec)

	line := strings.TrimSpace(buf.String())
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("one record must be exactly one JSONL line: %q", buf.String())
	}
	var got UsageRecord
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("JSONL line is not valid JSON: %v (%q)", err, line)
	}
	if got != rec {
		t.Fatalf("billing round-trip lost data:\n got %+v\nwant %+v", got, rec)
	}
}

// TestJSONLUsageLoggerOmitemptyBillingFlags verifies the billing-critical
// omitempty semantics: a central (metered) record omits the byok flag entirely,
// while a BYOK (unmetered) record carries byok:true so the billing sink can drop
// it. Cached follows the same rule. Getting these wrong either double-bills or
// silently un-bills.
func TestJSONLUsageLoggerOmitemptyBillingFlags(t *testing.T) {
	// Central, non-cached: byok/cached must be ABSENT (so a metered record never
	// looks like an unbillable one to a naive sink).
	central := marshalRec(t, UsageRecord{ID: "u1", Model: "m", Total: 10, CostUSD: 0.01})
	if strings.Contains(central, "byok") {
		t.Fatalf("central record must omit byok: %s", central)
	}
	if strings.Contains(central, "cached") {
		t.Fatalf("non-cached record must omit cached: %s", central)
	}

	// BYOK: byok:true must be present so billing drops it.
	byok := marshalRec(t, UsageRecord{ID: "u2", Model: "m", Total: 10, BYOK: true})
	if !strings.Contains(byok, `"byok":true`) {
		t.Fatalf("BYOK record must carry byok:true: %s", byok)
	}

	// Cached hit: cached:true present (auditable, unbilled).
	cached := marshalRec(t, UsageRecord{ID: "u3", Model: "m", Cached: true})
	if !strings.Contains(cached, `"cached":true`) {
		t.Fatalf("cache-hit record must carry cached:true: %s", cached)
	}
}

// TestJSONLUsageLoggerNeverLogsSecrets guards the "no key leakage in logs"
// security contract: the usage record carries only the key's LABEL and the
// account id, never a bearer token or provider secret. Even if a caller stuffs a
// secret-looking string into a non-secret field, the record type has no field
// that would carry an actual credential.
func TestJSONLUsageLoggerNeverLogsSecrets(t *testing.T) {
	var buf bytes.Buffer
	l := NewJSONLUsageLogger(&buf)
	// KeyName is a human label, not the token; confirm the token itself is not a
	// field on the record (compile-time guarantee reinforced at runtime here).
	l.Log(UsageRecord{ID: "u", KeyName: "prod-key", AccountID: "acct_1", Model: "m"})
	out := buf.String()
	for _, secret := range []string{"sk-", "Bearer ", "Authorization", "api_key", "apiKey"} {
		if strings.Contains(out, secret) {
			t.Fatalf("usage log leaked a credential-shaped token %q: %s", secret, out)
		}
	}
}

// TestJSONLUsageLoggerConcurrent verifies the logger's mutex serializes writers
// so concurrent Log calls (the real request hot path) each emit one intact,
// parseable JSONL line — no interleaved/torn records that would corrupt billing
// replay. Run under -race, this also catches an unsynchronized encoder.
func TestJSONLUsageLoggerConcurrent(t *testing.T) {
	var buf bytes.Buffer
	l := NewJSONLUsageLogger(&buf)
	const G, N = 16, 64
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < N; i++ {
				l.Log(UsageRecord{ID: "u", Model: "m", Total: 1, CostUSD: 0.001})
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != G*N {
		t.Fatalf("want %d intact JSONL lines, got %d", G*N, len(lines))
	}
	for i, ln := range lines {
		var r UsageRecord
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("line %d torn/interleaved (not valid JSON): %v — %q", i, err, ln)
		}
	}
}

// TestSetUsageLoggerNilIgnored: SetUsageLogger(nil) must not blank the sink (a
// nil sink would panic on the next request); the previous logger stays.
func TestSetUsageLoggerNilIgnored(t *testing.T) {
	s := &Server{usage: NopUsageLogger{}}
	s.SetUsageLogger(nil)
	if s.usage == nil {
		t.Fatal("SetUsageLogger(nil) must not clear the usage sink")
	}
	// A real logger replaces it.
	var buf bytes.Buffer
	jl := NewJSONLUsageLogger(&buf)
	s.SetUsageLogger(jl)
	if s.usage != jl {
		t.Fatal("SetUsageLogger(non-nil) must install the logger")
	}
}

func marshalRec(t *testing.T, rec UsageRecord) string {
	t.Helper()
	var buf bytes.Buffer
	NewJSONLUsageLogger(&buf).Log(rec)
	return buf.String()
}

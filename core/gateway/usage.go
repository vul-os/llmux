package gateway

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/vul-os/llmux/core/openai"
)

// UsageRecord is one logged request's usage and cost.
type UsageRecord struct {
	// ID is a unique per-record identifier used as an idempotency key by usage
	// sinks (e.g. cp) so retries dedupe and a record is billed at most once.
	ID         string  `json:"id"`
	Time       string  `json:"time"`
	KeyName    string  `json:"key,omitempty"`
	AccountID  string  `json:"account_id,omitempty"`
	Model      string  `json:"model"`
	Stream     bool    `json:"stream"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Total      int     `json:"total_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	Cached     bool    `json:"cached,omitempty"`
	// BYOK is true when the request was served with the account's OWN provider
	// key. Such usage is UNMETERED: it is recorded locally (JSONL/dashboard) for
	// the account's own visibility but is never billed to the control plane. The
	// zero value (false) is the central path, so existing records bill as before.
	BYOK bool `json:"byok,omitempty"`
}

// UsageLogger records per-request usage for billing/analytics.
type UsageLogger interface {
	Log(UsageRecord)
}

// NopUsageLogger discards records.
type NopUsageLogger struct{}

// Log implements UsageLogger.
func (NopUsageLogger) Log(UsageRecord) {}

// JSONLUsageLogger writes one JSON object per line (JSONL) to a writer.
type JSONLUsageLogger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONLUsageLogger builds a logger writing to w.
func NewJSONLUsageLogger(w io.Writer) *JSONLUsageLogger {
	return &JSONLUsageLogger{enc: json.NewEncoder(w)}
}

// Log implements UsageLogger.
func (l *JSONLUsageLogger) Log(rec UsageRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(rec)
}

// SetUsageLogger replaces the usage logger (e.g. with a JSONL file logger).
func (g *Gateway) SetUsageLogger(l UsageLogger) {
	if l != nil {
		g.usage = l
	}
}

// UsageLoggerOf returns the wired usage logger.
func (g *Gateway) UsageLoggerOf() UsageLogger { return g.usage }

// Stats returns the in-memory usage aggregator.
func (g *Gateway) Stats() *UsageStats { return g.stats }

// logUsage builds and emits a usage record from a response. It reads the key
// name and resolved account id from the request context so usage is attributed
// to the Vulos account, not just the key label.
func (g *Gateway) logUsage(ctx context.Context, model string, stream, cached bool, usage *openai.Usage) {
	rec := UsageRecord{
		ID:      GenID("usage-"),
		Time:    time.Now().UTC().Format(time.RFC3339),
		KeyName: keyName(ctx), AccountID: accountFrom(ctx),
		Model: model, Stream: stream, Cached: cached,
		BYOK: byokFrom(ctx),
	}
	if usage != nil {
		rec.Prompt = usage.PromptTokens
		rec.Completion = usage.CompletionTokens
		rec.Total = usage.TotalTokens
		if usage.Cost != nil {
			rec.CostUSD = usage.Cost.TotalCost
		}
	}
	g.stats.add(rec)
	g.usage.Log(rec)
}

// LogUsage records one request's usage. It is the exported form of logUsage,
// for dispatch paths outside this package (the HTTP shell's forwards).
func (g *Gateway) LogUsage(ctx context.Context, model string, stream, cached bool, usage *openai.Usage) {
	g.logUsage(ctx, model, stream, cached, usage)
}

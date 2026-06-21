package server

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/llmux/llmux/core/openai"
)

// UsageRecord is one logged request's usage and cost.
type UsageRecord struct {
	Time       string  `json:"time"`
	KeyName    string  `json:"key,omitempty"`
	Model      string  `json:"model"`
	Stream     bool    `json:"stream"`
	Prompt     int     `json:"prompt_tokens"`
	Completion int     `json:"completion_tokens"`
	Total      int     `json:"total_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	Cached     bool    `json:"cached,omitempty"`
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
func (s *Server) SetUsageLogger(l UsageLogger) {
	if l != nil {
		s.usage = l
	}
}

// logUsage builds and emits a usage record from a response.
func (s *Server) logUsage(keyName, model string, stream, cached bool, usage *openai.Usage) {
	rec := UsageRecord{
		Time:    time.Now().UTC().Format(time.RFC3339),
		KeyName: keyName, Model: model, Stream: stream, Cached: cached,
	}
	if usage != nil {
		rec.Prompt = usage.PromptTokens
		rec.Completion = usage.CompletionTokens
		rec.Total = usage.TotalTokens
		if usage.Cost != nil {
			rec.CostUSD = usage.Cost.TotalCost
		}
	}
	s.stats.add(rec)
	s.usage.Log(rec)
}

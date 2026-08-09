package server

import "github.com/vul-os/llmux/core/gateway"

// Usage recording lives in core/gateway (an embedding host meters too). These
// aliases keep the names cmd/llmux and integration/cp already use.

// UsageRecord is one logged request's usage and cost.
type UsageRecord = gateway.UsageRecord

// UsageLogger records per-request usage for billing/analytics.
type UsageLogger = gateway.UsageLogger

// NopUsageLogger discards records.
type NopUsageLogger = gateway.NopUsageLogger

// JSONLUsageLogger writes one JSON object per line (JSONL) to a writer.
type JSONLUsageLogger = gateway.JSONLUsageLogger

// NewJSONLUsageLogger builds a logger writing to w.
var NewJSONLUsageLogger = gateway.NewJSONLUsageLogger

// Aggregate accumulates usage counters.
type Aggregate = gateway.Aggregate

// Metrics holds the gateway's in-process counters.
type Metrics = gateway.Metrics

// SetUsageLogger replaces the usage logger (e.g. with a JSONL file logger).
func (s *Server) SetUsageLogger(l UsageLogger) { s.gw.SetUsageLogger(l) }

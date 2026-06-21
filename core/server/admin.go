package server

import (
	"net/http"
	"sync"
)

// Aggregate accumulates usage counters.
type Aggregate struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

func (a *Aggregate) add(rec UsageRecord) {
	a.Requests++
	a.PromptTokens += int64(rec.Prompt)
	a.CompletionTokens += int64(rec.Completion)
	a.TotalTokens += int64(rec.Total)
	a.CostUSD += rec.CostUSD
}

// maxUsageModels bounds distinct model entries in the in-memory usage map.
const maxUsageModels = 2000

// usageStats aggregates usage in memory for the /admin/usage endpoint.
type usageStats struct {
	mu      sync.Mutex
	total   Aggregate
	byKey   map[string]*Aggregate
	byModel map[string]*Aggregate
}

func newUsageStats() *usageStats {
	return &usageStats{byKey: map[string]*Aggregate{}, byModel: map[string]*Aggregate{}}
}

func (u *usageStats) add(rec UsageRecord) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.total.add(rec)
	key := rec.KeyName
	if key == "" {
		key = "(anonymous)"
	}
	if u.byKey[key] == nil {
		u.byKey[key] = &Aggregate{}
	}
	u.byKey[key].add(rec)
	// Bound by_model cardinality: model is client-supplied, so cap distinct
	// entries and bucket the overflow into "other" (prevents unbounded growth).
	model := rec.Model
	if u.byModel[model] == nil {
		if len(u.byModel) >= maxUsageModels {
			model = "other"
		}
		if u.byModel[model] == nil {
			u.byModel[model] = &Aggregate{}
		}
	}
	u.byModel[model].add(rec)
}

func (u *usageStats) snapshot() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	byKey := make(map[string]Aggregate, len(u.byKey))
	for k, v := range u.byKey {
		byKey[k] = *v
	}
	byModel := make(map[string]Aggregate, len(u.byModel))
	for k, v := range u.byModel {
		byModel[k] = *v
	}
	return map[string]any{
		"total":    u.total,
		"by_key":   byKey,
		"by_model": byModel,
	}
}

// keyStatus is a redacted key view for the admin listing.
type keyStatus struct {
	Name          string   `json:"name"`
	Key           string   `json:"key"`
	BudgetUSD     float64  `json:"budget_usd"`
	SpendUSD      float64  `json:"spend_usd"`
	RPM           int      `json:"rpm"`
	AllowedModels []string `json:"allowed_models,omitempty"`
}

// maskKey redacts a bearer token for display.
func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	if len(k) <= 10 {
		return k[:2] + "…"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	var out []keyStatus
	for _, k := range s.keys.Keys() {
		out = append(out, keyStatus{
			Name: k.Name, Key: maskKey(k.Key), BudgetUSD: k.BudgetUSD,
			SpendUSD: s.keys.Spend(k.Key), RPM: k.RPM, AllowedModels: k.AllowedModels,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.stats.snapshot())
}

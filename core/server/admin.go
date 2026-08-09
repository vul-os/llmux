package server

import (
	"net/http"
)

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
	store := s.gw.Keys()
	for _, k := range store.Keys() {
		out = append(out, keyStatus{
			Name: k.Name, Key: maskKey(k.Key), BudgetUSD: k.BudgetUSD,
			SpendUSD: store.Spend(k.Key), RPM: k.RPM, AllowedModels: k.AllowedModels,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.gw.Stats().Snapshot())
}

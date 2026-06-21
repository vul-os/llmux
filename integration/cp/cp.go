// Package cp is the OPTIONAL Vulos control-plane ("cp" / vulos-cloud) adapter
// for the llmux integration seam.
//
// It implements the server.Identity / server.BudgetGate / server.UsageLogger
// interfaces against the control plane. It is a SEPARATE package on purpose:
//
//   - The llmux core gateway never imports it (verify: `go list -deps` on the
//     core packages shows no integration/cp). Only the composition root
//     (cmd/llmux/main.go) references it, and only when LLMUX_CP_URL is set.
//   - Deleting this package must not break the standalone build. The core falls
//     back to its static-key defaults (server.staticIdentity / staticBudgetGate).
//
// All cp calls authenticate with the X-Relay-Auth header carrying the shared
// secret. The request/response shapes match the cp contract shared across the
// Vulos suite (vulos-office / vulos-mail).
package cp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/llmux/llmux/core/server"
)

// HeaderRelayAuth is the shared cp authentication header. Its value is the
// configured shared secret (LLMUX_CP_SECRET / cp_shared_secret).
const HeaderRelayAuth = "X-Relay-Auth"

// product is the cp product identifier for llmux usage/entitlements.
const product = "llm"

// Config holds the resolved cp adapter settings.
type Config struct {
	// BaseURL is the control-plane base URL (trailing slash trimmed).
	BaseURL string
	// Secret is the shared secret sent as X-Relay-Auth on every cp call.
	Secret string
}

// Enabled reports whether the cp adapter should be wired (a base URL is set).
func (c Config) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

// New builds a Config, normalizing the base URL.
func New(baseURL, secret string) Config {
	return Config{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Secret:  strings.TrimSpace(secret),
	}
}

func (c Config) client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func (c Config) auth(req *http.Request) {
	if c.Secret != "" {
		req.Header.Set(HeaderRelayAuth, c.Secret)
	}
}

// ---------------------------------------------------------------------------
// Identity: POST {cp}/api/llm/resolve {"key":"<token>"} -> {account_id,tier}
// ---------------------------------------------------------------------------

// Identity resolves a bearer token to a Vulos account via cp.
type Identity struct {
	cfg  Config
	http *http.Client
}

// NewIdentity builds the cp Identity resolver.
func NewIdentity(cfg Config) *Identity { return &Identity{cfg: cfg, http: cfg.client()} }

type resolveRequest struct {
	Key string `json:"key"`
}

type resolveResponse struct {
	AccountID string `json:"account_id"`
	Tier      string `json:"tier"`
}

// Resolve implements server.Identity. A 404 from cp means the token is unknown
// (ok=false → 401). Other transport/non-200 errors also resolve to unknown:
// an unauthenticated request should not be admitted just because cp blipped.
func (i *Identity) Resolve(ctx context.Context, token string) (server.Principal, bool) {
	raw, err := json.Marshal(resolveRequest{Key: token})
	if err != nil {
		return server.Principal{}, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.cfg.BaseURL+"/api/llm/resolve", bytes.NewReader(raw))
	if err != nil {
		return server.Principal{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	i.cfg.auth(req)

	resp, err := i.http.Do(req)
	if err != nil {
		return server.Principal{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return server.Principal{}, false
	}
	var r resolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.AccountID == "" {
		return server.Principal{}, false
	}
	return server.Principal{Token: token, AccountID: r.AccountID, Tier: r.Tier}, true
}

// ---------------------------------------------------------------------------
// BudgetGate: GET {cp}/api/entitlements?product=llm&account_id=<id>
//             -> {llm_enabled,llm_budget_usd,suspended}
// ---------------------------------------------------------------------------

// BudgetGate gates a request by the account's central LLM entitlements.
type BudgetGate struct {
	cfg  Config
	http *http.Client
}

// NewBudgetGate builds the cp BudgetGate.
func NewBudgetGate(cfg Config) *BudgetGate { return &BudgetGate{cfg: cfg, http: cfg.client()} }

type entitlementResponse struct {
	LLMEnabled   bool    `json:"llm_enabled"`
	LLMBudgetUSD float64 `json:"llm_budget_usd"`
	Suspended    bool    `json:"suspended"`
}

// Check implements server.BudgetGate.
//
// FAIL-OPEN: any transport error or non-200 from cp returns "allowed" so a cp
// outage never hard-downs the gateway. An EXPLICIT cp answer is enforced:
// denied when !llm_enabled || suspended || remaining<=0.
func (b *BudgetGate) Check(ctx context.Context, p server.Principal) server.BudgetDecision {
	reqURL := fmt.Sprintf("%s/api/entitlements?product=%s&account_id=%s",
		b.cfg.BaseURL, product, url.QueryEscape(p.AccountID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return server.BudgetDecision{} // fail open
	}
	b.cfg.auth(req)

	resp, err := b.http.Do(req)
	if err != nil {
		return server.BudgetDecision{} // fail open on transport error
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return server.BudgetDecision{} // fail open on cp error
	}
	var ent entitlementResponse
	if err := json.NewDecoder(resp.Body).Decode(&ent); err != nil {
		return server.BudgetDecision{} // fail open on malformed body
	}

	switch {
	case ent.Suspended:
		return server.BudgetDecision{Denied: true, Reason: "account suspended"}
	case !ent.LLMEnabled:
		return server.BudgetDecision{Denied: true, Reason: "llm not enabled for account"}
	case ent.LLMBudgetUSD <= 0:
		return server.BudgetDecision{Denied: true, Reason: "llm budget exhausted"}
	}
	return server.BudgetDecision{}
}

// ---------------------------------------------------------------------------
// Usage: POST {cp}/api/usage
//   {"product":"llm","account_id":..,"kind":"llm_tokens","count":..,"cost_usd":..}
// ---------------------------------------------------------------------------

// UsageLogger reports finalized per-request cost to cp (fire-and-forget).
type UsageLogger struct {
	cfg  Config
	http *http.Client
}

// NewUsageLogger builds the cp UsageLogger.
func NewUsageLogger(cfg Config) *UsageLogger { return &UsageLogger{cfg: cfg, http: cfg.client()} }

type usageBody struct {
	Product   string  `json:"product"`
	AccountID string  `json:"account_id"`
	Kind      string  `json:"kind"`
	Count     int     `json:"count"`
	CostUSD   float64 `json:"cost_usd"`
}

// Log implements server.UsageLogger. It POSTs the finalized cost to cp keyed by
// the resolved account id. Fire-and-forget: it never blocks the request path and
// silently drops on error. Records with no account id are skipped (nothing to
// attribute to cp).
func (u *UsageLogger) Log(rec server.UsageRecord) {
	if rec.AccountID == "" {
		return
	}
	body := usageBody{
		Product:   product,
		AccountID: rec.AccountID,
		Kind:      "llm_tokens",
		Count:     rec.Total,
		CostUSD:   rec.CostUSD,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	// Detach from the request lifetime; metering must outlive the response.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.BaseURL+"/api/usage", bytes.NewReader(raw))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		u.cfg.auth(req)
		resp, err := u.http.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// MultiUsageLogger fans a record out to several loggers, so the cp sink composes
// with the existing JSONL logger (JSONL logging is never removed).
type MultiUsageLogger struct {
	loggers []server.UsageLogger
}

// NewMultiUsageLogger composes loggers (nil entries are dropped).
func NewMultiUsageLogger(loggers ...server.UsageLogger) *MultiUsageLogger {
	out := make([]server.UsageLogger, 0, len(loggers))
	for _, l := range loggers {
		if l != nil {
			out = append(out, l)
		}
	}
	return &MultiUsageLogger{loggers: out}
}

// Log implements server.UsageLogger by delegating to each composed logger.
func (m *MultiUsageLogger) Log(rec server.UsageRecord) {
	for _, l := range m.loggers {
		l.Log(rec)
	}
}

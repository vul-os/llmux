package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/llmux/llmux/core/config"
)

// fakeBYOK is an in-test BYOKStore (the real encrypted store lives in core/byok;
// this keeps the server test free of KEK setup and focused on resolution).
type fakeBYOK struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

func newFakeBYOK() *fakeBYOK { return &fakeBYOK{m: map[string]map[string]string{}} }

func (f *fakeBYOK) Get(a, p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.m[a][p]
	return k, ok
}
func (f *fakeBYOK) Set(a, p, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m[a] == nil {
		f.m[a] = map[string]string{}
	}
	f.m[a][p] = k
	return nil
}
func (f *fakeBYOK) Clear(a, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m[a], p)
	return nil
}
func (f *fakeBYOK) Providers(a string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.m[a] {
		out = append(out, k)
	}
	return out
}

// keyCapturingUpstream returns a passthrough-shaped upstream that records the
// last bearer token it saw and replies with a metered chat completion.
func keyCapturingUpstream(t *testing.T, seen *string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000,"total_tokens":2000000}}`))
	}))
}

// newBYOKServer builds a server with a central-keyed passthrough provider, a
// wildcard route, an account-resolving identity, a capture logger, and a BYOK
// store.
func newBYOKServer(t *testing.T, up *httptest.Server, account string, store BYOKStore) (*Server, *captureLogger) {
	t.Helper()
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: up.URL + "/v1", APIKey: "central-key"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	priceModel(t, s, "openai/gpt-4o")
	s.SetIdentity(stubIdentity{account: account, ok: true})
	s.SetBudgetGate(stubBudget{})
	s.SetBYOKStore(store)
	cl := &captureLogger{}
	s.usage = cl
	return s, cl
}

// TestBYOKResolutionUsesAccountKeyUnmetered: an account with a BYOK key for the
// routed provider has the request served with ITS OWN key and recorded UNMETERED.
func TestBYOKResolutionUsesAccountKeyUnmetered(t *testing.T) {
	var seen string
	var mu sync.Mutex
	up := keyCapturingUpstream(t, &seen, &mu)
	defer up.Close()

	store := newFakeBYOK()
	store.Set("acct_1", "openai", "byok-account-key")
	s, cl := newBYOKServer(t, up, "acct_1", store)

	rec := doPost(s, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotKey := seen
	mu.Unlock()
	if gotKey != "byok-account-key" {
		t.Fatalf("provider should receive the account's BYOK key, got %q", gotKey)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(cl.recs))
	}
	if !cl.recs[0].BYOK {
		t.Fatalf("BYOK request must be recorded unmetered (BYOK=true): %+v", cl.recs[0])
	}
	if cl.recs[0].AccountID != "acct_1" {
		t.Fatalf("account not attributed: %+v", cl.recs[0])
	}
}

// TestCentralResolutionMetered: an account with NO BYOK key for the provider is
// served with the CENTRAL key and recorded as metered (BYOK=false).
func TestCentralResolutionMetered(t *testing.T) {
	var seen string
	var mu sync.Mutex
	up := keyCapturingUpstream(t, &seen, &mu)
	defer up.Close()

	s, cl := newBYOKServer(t, up, "acct_2", newFakeBYOK()) // empty store
	rec := doPost(s, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	gotKey := seen
	mu.Unlock()
	if gotKey != "central-key" {
		t.Fatalf("provider should receive the central key, got %q", gotKey)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(cl.recs))
	}
	if cl.recs[0].BYOK {
		t.Fatalf("central request must be metered (BYOK=false): %+v", cl.recs[0])
	}
	if cl.recs[0].CostUSD <= 0 {
		t.Fatalf("central request must carry cost: %+v", cl.recs[0])
	}
}

// TestBYOKDisabledNoStore: with no BYOK store, even a request that looks like it
// should be BYOK falls back to the central key and is metered.
func TestBYOKDisabledNoStore(t *testing.T) {
	var seen string
	var mu sync.Mutex
	up := keyCapturingUpstream(t, &seen, &mu)
	defer up.Close()

	s, cl := newBYOKServer(t, up, "acct_3", nil) // SetBYOKStore(nil) -> disabled
	rec := doPost(s, "/v1/chat/completions", `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	mu.Lock()
	gotKey := seen
	mu.Unlock()
	if gotKey != "central-key" {
		t.Fatalf("disabled BYOK must use central key, got %q", gotKey)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.recs[0].BYOK {
		t.Fatal("disabled BYOK must record metered")
	}
}

// TestBYOKEligibilityBedrockCentralOnly: Bedrock authenticates with AWS SigV4,
// not a single key, so it is never BYOK-eligible even when a key is stored.
func TestBYOKEligibilityBedrockCentralOnly(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "y")
	cfg := &config.Config{
		Server: config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{
			{Name: "openai", Type: config.TypePassthrough, BaseURL: "http://x/v1", APIKey: "c"},
			{Name: "br", Type: config.TypeBedrock, BaseURL: "http://x"},
		},
		Routes: []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if s.byokEligible("br") {
		t.Fatal("bedrock must be central-only")
	}
	if !s.byokEligible("openai") {
		t.Fatal("passthrough must be BYOK-eligible")
	}
	if !s.byokEligible("unknown") {
		t.Fatal("unknown providers default to eligible")
	}
}

// TestBYOKAdminEndpoints exercises set/list/clear and the no-key-leak guarantee.
func TestBYOKAdminEndpoints(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "master"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: "http://x/v1", APIKey: "c"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.SetBYOKStore(newFakeBYOK())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer master")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Set a BYOK key.
	if rec := do("PUT", "/admin/byok/acct_1/openai", `{"api_key":"sk-mine"}`); rec.Code != 200 {
		t.Fatalf("set status=%d body=%s", rec.Code, rec.Body.String())
	}
	// List must include the provider but NEVER the key value.
	rec := do("GET", "/admin/byok/acct_1", "")
	if rec.Code != 200 {
		t.Fatalf("list status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openai") {
		t.Fatalf("list should mention provider: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-mine") {
		t.Fatalf("list must NOT leak the key: %s", rec.Body.String())
	}
	// Clear.
	if rec := do("DELETE", "/admin/byok/acct_1/openai", ""); rec.Code != 200 {
		t.Fatalf("clear status=%d", rec.Code)
	}
}

// TestBYOKAdminRequiresMasterKey: BYOK admin endpoints are master-key gated.
func TestBYOKAdminRequiresMasterKey(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0", MasterKey: "master"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: "http://x/v1", APIKey: "c"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, _ := New(cfg)
	s.SetBYOKStore(newFakeBYOK())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/admin/byok/acct_1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated BYOK admin must be 401, got %d", rec.Code)
	}
}

// TestBYOKAdminDisabled: with no store, the endpoints report 501.
func TestBYOKAdminDisabled(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: "http://x/v1", APIKey: "c"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "openai"}},
	}
	s, _ := New(cfg)
	rec := httptest.NewRecorder()
	// Keyless /admin is loopback-only (fail closed); present as a local caller.
	req := httptest.NewRequest("GET", "/admin/byok/acct_1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("disabled BYOK admin must be 501, got %d", rec.Code)
	}
}

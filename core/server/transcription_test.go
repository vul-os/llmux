package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/sovereign"
)

// buildTranscription constructs a multipart/form-data body with an audio file
// and a "model" field, mirroring what an OpenAI SDK sends to
// /v1/audio/transcriptions. Returns the body bytes and the multipart content
// type (with boundary).
func buildTranscription(t *testing.T, model, filename string, audio []byte, extra map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if model != "" {
		if err := mw.WriteField("model", model); err != nil {
			t.Fatalf("write model field: %v", err)
		}
	}
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// postMultipart posts a multipart body to path with the given bearer key ("" for
// no auth) and content type.
func postMultipart(s *Server, path, key string, body []byte, contentType string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// transcribeUpstream is a fake OpenAI-shaped transcription upstream. It records
// what it received (the parsed model form field, the file bytes, the auth
// header) and replies with a Whisper-style JSON body (NO usage object). When
// usageJSON is non-empty it is embedded so the gpt-4o-transcribe metered path
// can be exercised.
type transcribeUpstream struct {
	srv       *httptest.Server
	gotModel  string
	gotFile   []byte
	gotAuth   string
	gotPath   string
	gotCT     string
	hits      int32
	usageJSON string
}

func newTranscribeUpstream(t *testing.T, usageJSON string) *transcribeUpstream {
	t.Helper()
	u := &transcribeUpstream{usageJSON: usageJSON}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&u.hits, 1)
		u.gotPath = r.URL.Path
		u.gotAuth = r.Header.Get("Authorization")
		u.gotCT = r.Header.Get("Content-Type")
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				if part.FormName() == "model" && part.FileName() == "" {
					u.gotModel = strings.TrimSpace(string(data))
				} else if part.FileName() != "" {
					u.gotFile = data
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"text":"hello world"`
		if u.usageJSON != "" {
			body += `,"usage":` + u.usageJSON
		}
		body += `}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// transcribeServer builds a server routing "*" (or an alias) to a passthrough
// provider pointed at up. keys, if non-empty, configures a static key.
func transcribeServer(t *testing.T, upURL string, routes []config.RouteConfig, keys []config.KeyConfig) (*Server, *captureLogger) {
	t.Helper()
	if routes == nil {
		routes = []config.RouteConfig{{Model: "*", Provider: "openai"}}
	}
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "openai", Type: config.TypePassthrough, BaseURL: upURL + "/v1", APIKey: "central-key"}},
		Routes:    routes,
		Keys:      keys,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	cl := &captureLogger{}
	s.usage = cl
	return s, cl
}

// TestTranscription_ForwardedModelRewrittenFilePreserved is the happy path: a
// multipart upload is proxied to the provider, the model form field is rewritten
// to the route's target, the audio file is preserved byte-for-byte, provider
// auth is injected, and the response is relayed. A $0 auditable usage line is
// logged (Whisper reports no usage).
func TestTranscription_ForwardedModelRewrittenFilePreserved(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, cl := transcribeServer(t, up.srv.URL,
		[]config.RouteConfig{{Model: "whisper-1", Provider: "openai", TargetModel: "whisper-1-real"}}, nil)

	audio := []byte("RIFF....fake-wav-bytes....")
	body, ct := buildTranscription(t, "whisper-1", "clip.wav", audio, map[string]string{"language": "en"})
	rec := postMultipart(s, "/v1/audio/transcriptions", "", body, ct)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if up.gotPath != "/v1/audio/transcriptions" {
		t.Fatalf("upstream path=%q", up.gotPath)
	}
	if up.gotModel != "whisper-1-real" {
		t.Fatalf("model not rewritten to target: %q", up.gotModel)
	}
	if !bytes.Equal(up.gotFile, audio) {
		t.Fatalf("audio file not preserved: got %q", up.gotFile)
	}
	if up.gotAuth != "Bearer central-key" {
		t.Fatalf("provider auth not injected: %q", up.gotAuth)
	}
	if !strings.HasPrefix(up.gotCT, "multipart/form-data") {
		t.Fatalf("forwarded content-type not multipart: %q", up.gotCT)
	}
	if !strings.Contains(rec.Body.String(), "hello world") {
		t.Fatalf("response not relayed: %s", rec.Body.String())
	}
	// A served transcription must be logged (auditable), even at $0.
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("expected 1 auditable usage record, got %d", len(cl.recs))
	}
	if cl.recs[0].Model != "whisper-1" {
		t.Fatalf("usage record model=%q, want whisper-1", cl.recs[0].Model)
	}
	if cl.recs[0].CostUSD != 0 {
		t.Fatalf("Whisper has no catalog price; expected $0 auditable line, got %v", cl.recs[0].CostUSD)
	}
}

// TestTranscription_TranslationsRoute: /v1/audio/translations is wired too.
func TestTranscription_TranslationsRoute(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, _ := transcribeServer(t, up.srv.URL, nil, nil)
	body, ct := buildTranscription(t, "whisper-1", "clip.mp3", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/translations", "", body, ct)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if up.gotPath != "/v1/audio/translations" {
		t.Fatalf("upstream path=%q", up.gotPath)
	}
}

// TestTranscription_MissingModel: a multipart body without a model field is a 400.
func TestTranscription_MissingModel(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, _ := transcribeServer(t, up.srv.URL, nil, nil)
	body, ct := buildTranscription(t, "", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "", body, ct)
	if rec.Code != 400 {
		t.Fatalf("status=%d, want 400 (missing model)", rec.Code)
	}
	if atomic.LoadInt32(&up.hits) != 0 {
		t.Fatalf("missing-model request must not reach upstream")
	}
}

// TestTranscription_NonMultipart: a JSON body is rejected with a clear 400 (these
// endpoints require multipart/form-data).
func TestTranscription_NonMultipart(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, _ := transcribeServer(t, up.srv.URL, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader(`{"model":"whisper-1"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d, want 400 (non-multipart)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_content_type") {
		t.Fatalf("expected invalid_content_type, got %s", rec.Body.String())
	}
	if atomic.LoadInt32(&up.hits) != 0 {
		t.Fatalf("non-multipart request must not reach upstream")
	}
}

// TestTranscription_BudgetedUnpricedRefusedPreflight is THE money-boundary
// regression: a BUDGETED key requesting an unpriceable transcription model must
// be refused BEFORE any upstream call. Whisper has no per-audio-minute price in
// the catalog, so serving it would record $0, never decrement the budget, and
// let a budgeted key burn unbounded real audio spend (the same fail-open class
// hardened for chat). It must fail CLOSED.
func TestTranscription_BudgetedUnpricedRefusedPreflight(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, cl := transcribeServer(t, up.srv.URL, nil,
		[]config.KeyConfig{{Key: "sk-budget", Name: "tenant", BudgetUSD: 100}})

	body, ct := buildTranscription(t, "whisper-1", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "sk-budget", body, ct)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("unpriced audio on a budgeted key must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_priced") {
		t.Fatalf("expected model_not_priced, got %s", rec.Body.String())
	}
	if n := atomic.LoadInt32(&up.hits); n != 0 {
		t.Fatalf("refusal must happen BEFORE any upstream audio spend, hits=%d", n)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 0 {
		t.Fatalf("a refused request must record no usage, got %d", len(cl.recs))
	}
}

// TestTranscription_UnlimitedKeyServedAuditedZeroCost: an UNLIMITED key (no
// budget) is served and logged at $0 — the known per-audio-minute gap, but an
// auditable line, never a silent free path.
func TestTranscription_UnlimitedKeyServedAuditedZeroCost(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, cl := transcribeServer(t, up.srv.URL, nil,
		[]config.KeyConfig{{Key: "sk-unlimited", Name: "tenant", BudgetUSD: 0}})

	body, ct := buildTranscription(t, "whisper-1", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "sk-unlimited", body, ct)
	if rec.Code != 200 {
		t.Fatalf("unlimited key must be served, got %d: %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&up.hits) != 1 {
		t.Fatalf("unlimited-key request must reach upstream once")
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 || cl.recs[0].CostUSD != 0 {
		t.Fatalf("expected 1 auditable $0 record, got %+v", cl.recs)
	}
}

// TestTranscription_MeteredWhenUpstreamReportsUsage: when a provider DOES report
// a usage object (e.g. gpt-4o-transcribe) AND the model is priced, the
// transcription meters a real non-zero cost through the same path as chat.
func TestTranscription_MeteredWhenUpstreamReportsUsage(t *testing.T) {
	up := newTranscribeUpstream(t, `{"prompt_tokens":1000000,"completion_tokens":0,"total_tokens":1000000}`)
	s, cl := transcribeServer(t, up.srv.URL, nil,
		[]config.KeyConfig{{Key: "sk-budget", Name: "tenant", BudgetUSD: 100}})
	priceModel(t, s, "gpt-4o-transcribe")

	body, ct := buildTranscription(t, "gpt-4o-transcribe", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "sk-budget", body, ct)
	if rec.Code != 200 {
		t.Fatalf("priced+metered transcription must be served, got %d: %s", rec.Code, rec.Body.String())
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(cl.recs))
	}
	if cl.recs[0].Total != 1000000 || cl.recs[0].CostUSD <= 0 {
		t.Fatalf("upstream-reported usage must be metered non-zero: %+v", cl.recs[0])
	}
}

// TestTranscription_SovereigntyBlocked: a non-local provider without opt-in must
// be denied on the audio route too — no socket may open.
func TestTranscription_SovereigntyBlocked(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "remote", Type: config.TypePassthrough, BaseURL: "https://api.remote.example/v1", APIKey: "k", AllowEgress: false}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "remote"}},
	}
	// Sanity: the provider is genuinely classified as blocked egress.
	if d := sovereign.NewPolicy(cfg.Providers).Check("remote"); d.Allowed {
		t.Fatalf("test precondition: remote provider should be blocked, got allowed")
	}
	s, _ := New(cfg)
	body, ct := buildTranscription(t, "whisper-1", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "", body, ct)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked audio egress must be 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Error.Code != "egress_not_allowed" {
		t.Fatalf("expected egress_not_allowed, got %q (%s)", e.Error.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&up.hits) != 0 {
		t.Fatalf("blocked audio must not reach any network")
	}
}

// TestTranscription_UnsupportedByAdapter: a translating (non-Forwarder) adapter
// returns 501 for the audio route.
func TestTranscription_UnsupportedByAdapter(t *testing.T) {
	cfg := &config.Config{
		Server:    config.ServerConfig{Addr: ":0"},
		Providers: []config.ProviderConfig{{Name: "anthropic", Type: config.TypeAnthropic, APIKey: "k"}},
		Routes:    []config.RouteConfig{{Model: "*", Provider: "anthropic"}},
	}
	s, _ := New(cfg)
	body, ct := buildTranscription(t, "whisper-1", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "", body, ct)
	if rec.Code != 501 {
		t.Fatalf("status=%d, want 501 (adapter has no Forwarder)", rec.Code)
	}
}

// TestTranscription_ModelAllowListDenied: a key whose model allow-list excludes
// the requested transcription model is denied 403 before any upstream call.
func TestTranscription_ModelAllowListDenied(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, _ := transcribeServer(t, up.srv.URL, nil,
		[]config.KeyConfig{{Key: "sk-scoped", Name: "tenant", AllowedModels: []string{"gpt-4o"}}})
	body, ct := buildTranscription(t, "whisper-1", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "sk-scoped", body, ct)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (model not allowed)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("expected model_not_allowed, got %s", rec.Body.String())
	}
	if atomic.LoadInt32(&up.hits) != 0 {
		t.Fatalf("disallowed model must not reach upstream")
	}
}

// TestTranscription_BYOKUnmeteredUsesAccountKey: an account with a BYOK key for
// the routed provider has its transcription served with ITS OWN key and recorded
// UNMETERED (BYOK=true) — the audio path honors the same BYOK contract as chat.
func TestTranscription_BYOKUnmeteredUsesAccountKey(t *testing.T) {
	up := newTranscribeUpstream(t, "")
	s, cl := transcribeServer(t, up.srv.URL, nil, nil)
	s.SetIdentity(stubIdentity{account: "acct_1", ok: true})
	s.SetBudgetGate(stubBudget{})
	store := newFakeBYOK()
	store.Set("acct_1", "openai", "byok-account-key")
	s.SetBYOKStore(store)

	body, ct := buildTranscription(t, "whisper-1", "clip.wav", []byte("audio"), nil)
	rec := postMultipart(s, "/v1/audio/transcriptions", "any-token", body, ct)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if up.gotAuth != "Bearer byok-account-key" {
		t.Fatalf("provider must receive the account's BYOK key, got %q", up.gotAuth)
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.recs) != 1 || !cl.recs[0].BYOK {
		t.Fatalf("BYOK transcription must be recorded unmetered (BYOK=true): %+v", cl.recs)
	}
}

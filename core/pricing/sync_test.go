package pricing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// openRouterBody is an OpenRouter-shaped /models response.
const openRouterBody = `{"data":[
	{"id":"openai/gpt-4o","context_length":128000,"pricing":{"prompt":"0.0000025","completion":"0.00001"},"top_provider":{"max_completion_tokens":16384},"architecture":{"modality":"text+image->text"},"supported_parameters":["tools"]},
	{"id":"shared/model","context_length":1000,"pricing":{"prompt":"0.000002","completion":"0.000002"}}
]}`

// liteLLMBody is a LiteLLM-shaped model_prices response.
const liteLLMBody = `{
	"sample_spec":{"input_cost_per_token":0},
	"litellm-only":{"max_input_tokens":64000,"max_output_tokens":8192,"input_cost_per_token":0.00000027,"output_cost_per_token":0.0000011,"litellm_provider":"deepseek","mode":"chat","supports_function_calling":true},
	"shared/model":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000001,"litellm_provider":"shared","mode":"chat"}
}`

// jsonServer stands up an httptest server returning body with status 200.
func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSyncOnceMergesSourcesAndPrecedence(t *testing.T) {
	or := jsonServer(t, openRouterBody)
	ll := jsonServer(t, liteLLMBody)

	cat := New()
	src := []Source{
		NewOpenRouterSource(or.URL),
		NewLiteLLMSource(ll.URL),
	}
	s := NewSyncer(cat, src, 0, "")
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	// Models from OpenRouter present.
	if _, ok := cat.Get("openai/gpt-4o"); !ok {
		t.Fatal("missing openrouter model openai/gpt-4o")
	}
	// Models from LiteLLM present.
	if _, ok := cat.Get("litellm-only"); !ok {
		t.Fatal("missing litellm model litellm-only")
	}

	// Precedence: both sources define "shared/model". LiteLLM (priority 20)
	// beats OpenRouter (priority 30) in the merged best view.
	p, ok := cat.Get("shared/model")
	if !ok {
		t.Fatal("missing shared/model")
	}
	// LiteLLM price is 1/MTok; OpenRouter price is 2/MTok.
	if p.InputPerMTok != 1 {
		t.Fatalf("shared/model input=%v, want 1 (litellm wins)", p.InputPerMTok)
	}
	if p.Provider != "shared" {
		t.Fatalf("shared/model provider=%q, want shared (litellm wins)", p.Provider)
	}
}

func TestSyncOnceWritesCache(t *testing.T) {
	or := jsonServer(t, openRouterBody)
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")

	cat := New()
	s := NewSyncer(cat, []Source{NewOpenRouterSource(or.URL)}, 0, path)
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	// And it loads back into a fresh catalog with the synced model.
	c2 := New()
	if err := c2.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := c2.Get("openai/gpt-4o"); !ok {
		t.Fatal("cached catalog missing synced model")
	}
}

func TestSyncOnceErrorDoesNotAbortOthers(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := jsonServer(t, liteLLMBody)

	cat := New()
	// Bad source first so we exercise "continue after error".
	s := NewSyncer(cat, []Source{
		NewOpenRouterSource(bad.URL),
		NewLiteLLMSource(good.URL),
	}, 0, "")

	err := s.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("expected firstErr from the 500 source")
	}
	// The good source must still have been merged.
	if _, ok := cat.Get("litellm-only"); !ok {
		t.Fatal("good source not merged despite a failing source")
	}
}

func TestHTTPSourceFetchNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := NewOpenRouterSource(srv.URL)
	if _, err := src.Fetch(context.Background()); err == nil {
		t.Fatal("expected error on non-200")
	}
}

func TestHTTPSourceFetchOK(t *testing.T) {
	srv := jsonServer(t, openRouterBody)
	src := NewOpenRouterSource(srv.URL)
	prices, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, ok := prices["openai/gpt-4o"]; !ok {
		t.Fatalf("parsed map missing model: %v", prices)
	}
}

func TestParseAutoByURL(t *testing.T) {
	// URL hints route to the right named source.
	if got := SourceFromURL("https://openrouter.ai/api/v1/models").Name(); got != "openrouter" {
		t.Errorf("openrouter URL -> %q", got)
	}
	if got := SourceFromURL("https://x/model_prices_and_context_window.json").Name(); got != "litellm" {
		t.Errorf("model_prices URL -> %q", got)
	}
	if got := SourceFromURL("https://raw.githubusercontent.com/litellm/x.json").Name(); got != "litellm" {
		t.Errorf("litellm URL -> %q", got)
	}
}

func TestParseAutoUnknownURLShapeDetection(t *testing.T) {
	// Unknown host with an OpenRouter-shaped body parses as openrouter.
	orSrv := jsonServer(t, openRouterBody)
	src := SourceFromURL(orSrv.URL) // unknown host (127.0.0.1)
	prices, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch openrouter-shaped: %v", err)
	}
	if _, ok := prices["openai/gpt-4o"]; !ok {
		t.Fatalf("openrouter-shaped body not parsed: %v", prices)
	}

	// Unknown host with a LiteLLM-shaped body falls through to litellm parse.
	llSrv := jsonServer(t, liteLLMBody)
	src2 := SourceFromURL(llSrv.URL)
	prices2, err := src2.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch litellm-shaped: %v", err)
	}
	if _, ok := prices2["litellm-only"]; !ok {
		t.Fatalf("litellm-shaped body not parsed: %v", prices2)
	}
}

func TestPerTokenToMTok(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"0.0000025", 2.5},
		{" 0.000001 ", 1},
		{"", 0},
		{"not-a-number", 0},
	}
	for _, c := range cases {
		if got := perTokenToMTok(c.in); got != c.want {
			t.Errorf("perTokenToMTok(%q)=%v, want %v", c.in, got, c.want)
		}
	}
}

func TestUnitMultiplier(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1K", 1000},
		{"1k tokens", 1000},
		{"1M", 1},
		{"1M tokens", 1},
		{"1", 1e6},
		{"1 unit", 1e6},
		{"per-something", 1000}, // unknown defaults to per-1K
		{"", 1000},
	}
	for _, c := range cases {
		if got := unitMultiplier(c.in); got != c.want {
			t.Errorf("unitMultiplier(%q)=%v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseAzureInputOutputMetersAndSkip(t *testing.T) {
	data := []byte(`{"Items":[
		{"unitPrice":0.0025,"meterName":"gpt-4o Input Global Tokens","unitOfMeasure":"1K"},
		{"unitPrice":0.01,"meterName":"gpt-4o Output Global Tokens","unitOfMeasure":"1K"},
		{"unitPrice":1.0,"meterName":"Fine Tuning Training","unitOfMeasure":"1"}
	]}`)
	prices, err := ParseAzure(data)
	if err != nil {
		t.Fatalf("ParseAzure: %v", err)
	}
	p, ok := prices["azure/gpt-4o"]
	if !ok {
		t.Fatalf("azure/gpt-4o missing: %+v", prices)
	}
	// Both input and output meters accumulate onto the same model.
	if p.InputPerMTok != 2.5 {
		t.Errorf("input=%v, want 2.5", p.InputPerMTok)
	}
	if p.OutputPerMTok != 10 {
		t.Errorf("output=%v, want 10", p.OutputPerMTok)
	}
	// A meter that is neither input nor output is skipped entirely.
	for id := range prices {
		if id != "azure/gpt-4o" {
			t.Errorf("unexpected non-token meter parsed: %q", id)
		}
	}
}

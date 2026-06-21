package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUsage checks the help/usage text covers every subcommand and the default
// address, since it is what users see on `llmux help` and unknown subcommands.
func TestUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	usage(f)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{"serve", "version", "models", "catalog", "keys", "help", defaultAddr} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q\n%s", want, out)
		}
	}
}

// TestVersionConstant guards the version string the `version` subcommand prints.
func TestVersionConstant(t *testing.T) {
	if version == "" {
		t.Fatal("version must be non-empty")
	}
}

// TestHTTPGetMalformedJSON exercises the decode error path of httpGet.
func TestHTTPGetMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	var v map[string]any
	if err := httpGet(srv.URL, "/x", "", &v); err == nil {
		t.Fatal("expected decode error on malformed JSON")
	}
}

// TestHTTPGetConnRefused exercises the transport error path (no server).
func TestHTTPGetConnRefused(t *testing.T) {
	var v any
	// Closed/unused address: the request must fail to connect.
	if err := httpGet("http://127.0.0.1:0", "/x", "", &v); err == nil {
		t.Fatal("expected transport error against an unreachable address")
	}
}

// TestHTTPGetSendsBearer confirms the Authorization header is set when a key is
// supplied (the auth seam used by the keys subcommand).
func TestHTTPGetSendsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var v map[string]any
	if err := httpGet(srv.URL, "/admin/keys", "secret", &v); err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization=%q, want %q", gotAuth, "Bearer secret")
	}
}

// TestFetchModelsError surfaces upstream non-200 through fetchModels.
func TestFetchModelsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := fetchModels(srv.URL, &buf); err == nil {
		t.Fatal("expected error from fetchModels on 502")
	}
}

// TestFetchCatalogError surfaces upstream non-200 through fetchCatalog.
func TestFetchCatalogError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := fetchCatalog(srv.URL, &buf); err == nil {
		t.Fatal("expected error from fetchCatalog on 503")
	}
}

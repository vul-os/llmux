package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchModels(t *testing.T) {
	const body = `{
		"object": "list",
		"data": [
			{"id": "gpt-4o", "object": "model", "owned_by": "openai",
			 "input_price_per_mtok": 2.5, "output_price_per_mtok": 10, "context_window": 128000},
			{"id": "claude-sonnet", "object": "model", "owned_by": "anthropic",
			 "input_price_per_mtok": 3, "output_price_per_mtok": 15, "context_window": 200000}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := fetchModels(srv.URL, &buf); err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "INPUT $/Mtok", "gpt-4o", "2.50", "10.00", "128000", "claude-sonnet", "200000"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestFetchCatalog(t *testing.T) {
	const body = `{"updated": "2026-06-19T12:00:00Z", "count": 42, "prices": {}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/catalog.json" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := fetchCatalog(srv.URL, &buf); err != nil {
		t.Fatalf("fetchCatalog: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "42") {
		t.Errorf("missing count: %s", out)
	}
	if !strings.Contains(out, "2026-06-19T12:00:00Z") {
		t.Errorf("missing updated: %s", out)
	}
}

func TestFetchKeys(t *testing.T) {
	const body = `{"keys": [
		{"name": "alice", "key": "sk-llm…cdef", "budget_usd": 100, "spend_usd": 12.5, "rpm": 60},
		{"name": "bob", "key": "sk-llm…0099", "budget_usd": 0, "spend_usd": 0, "rpm": 0}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/keys" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer master-key" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := fetchKeys(srv.URL, "master-key", &buf); err != nil {
		t.Fatalf("fetchKeys: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "alice", "sk-llm…cdef", "100.00", "12.50", "60", "bob"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestFetchKeysNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("expected no auth header, got %q", got)
		}
		_, _ = w.Write([]byte(`{"keys": []}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := fetchKeys(srv.URL, "", &buf); err != nil {
		t.Fatalf("fetchKeys: %v", err)
	}
}

func TestHTTPGetNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	var v any
	err := httpGet(srv.URL, "/admin/keys", "", &v)
	if err == nil {
		t.Fatal("expected error on non-200")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status: %v", err)
	}
}

package config

// Library mode and the host's environment.
//
// FromJSON is what an embedder calls: the C ABI (llmux_new) hands it the
// caller's JSON document, and so does an in-process Go host. The process
// environment in that situation belongs to the HOST APPLICATION, which never
// heard of llmux. Two rules follow, and these tests hold them:
//
//  1. A value the document states wins over the environment.
//  2. DATABASE_URL and VULOS_DATABASE_URL are not read at all.
//
// Rule 2 is the one with teeth. cfg.Postgres is the single field that turns
// gateway.New from an inert construction into remote I/O: it builds the
// Postgres key store, which connects and runs CREATE SCHEMA / CREATE TABLE
// immediately. Before this, a Rails or Django app with DATABASE_URL set that
// loaded libllmux got llmux tables in its production database — while
// ffi/include/llmux.h promised the gateway was inert "unless your configuration
// names a Postgres DSN".
//
// Load — `llmux serve` reading a file an operator wrote, in a process the
// operator started — keeps the historical env-last rules. TestPostgresDSNResolution
// covers that side and must keep passing.

import "testing"

func TestLibraryModeIgnoresTheHostsUnnamespacedDSNs(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "VULOS_DATABASE_URL"} {
		t.Run(name, func(t *testing.T) {
			clearKnownProviderEnv(t)
			clearLLMUXEnv(t)
			t.Setenv(name, "postgres://host-app-production/appdb")

			c, err := FromJSON([]byte(`{}`))
			if err != nil {
				t.Fatalf("FromJSON: %v", err)
			}
			if c.Postgres != "" {
				t.Errorf("FromJSON adopted %s (Postgres = %q).\n"+
					"  That variable is the HOST application's, not llmux's. Adopting it makes "+
					"gateway.New connect to their production database and run CREATE SCHEMA / "+
					"CREATE TABLE in it, which is the opposite of the inertness llmux.h promises.",
					name, c.Postgres)
			}
			if c.PostgresSchema != "" {
				t.Errorf("PostgresSchema = %q, want empty — the schema default fired, so something "+
					"still thinks Postgres is configured", c.PostgresSchema)
			}
		})
	}
}

// The namespaced variable is unambiguous: nobody exports LLMUX_POSTGRES by
// accident. Ignoring the shared DSNs must not become "library mode reads no
// environment at all" — that would break every embedder configuring llmux the
// documented way.
func TestLibraryModeStillHonoursTheNamespacedDSN(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)
	t.Setenv("LLMUX_POSTGRES", "postgres://meant-for-llmux/db")

	c, err := FromJSON(nil)
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if c.Postgres != "postgres://meant-for-llmux/db" {
		t.Errorf("Postgres = %q, want the LLMUX_POSTGRES value — the namespaced variable is the "+
			"documented way to configure an embedder from the environment", c.Postgres)
	}
	if c.PostgresSchema != "llmux" {
		t.Errorf("PostgresSchema = %q, want the llmux default once a DSN is in play", c.PostgresSchema)
	}
}

func TestLibraryDocumentBeatsTheEnvironment(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)
	t.Setenv("LLMUX_POSTGRES", "postgres://from-the-environment/db")
	t.Setenv("LLMUX_ADDR", ":9999")
	t.Setenv("LLMUX_LOG_LEVEL", "debug")
	t.Setenv("LLMUX_CP_URL", "https://cp.from.env")
	t.Setenv("LLMUX_REDIS", "10.0.0.1:6379")

	doc := []byte(`{
	  "postgres": "postgres://stated-by-the-caller/db",
	  "server": {"addr": ":7000"},
	  "log_level": "warn",
	  "cp": {"cp_url": "https://cp.stated.by.caller"},
	  "redis": "127.0.0.1:6379"
	}`)
	c, err := FromJSON(doc)
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	for _, tc := range []struct{ field, got, want string }{
		{"postgres", c.Postgres, "postgres://stated-by-the-caller/db"},
		{"server.addr", c.Server.Addr, ":7000"},
		{"log_level", c.LogLevel, "warn"},
		{"cp.cp_url", c.CP.URL, "https://cp.stated.by.caller"},
		{"redis", c.Redis, "127.0.0.1:6379"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q — the environment overwrote a value the caller stated "+
				"explicitly in its document", tc.field, tc.got, tc.want)
		}
	}
}

// "The caller said nothing" and "the caller said no" are different answers, and
// only the raw document can tell them apart: both decode to the empty string.
// An embedder that writes "postgres": "" is refusing a database, and the host's
// environment must not overrule that.
func TestLibraryAnExplicitEmptyValueIsStillAStatement(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)
	t.Setenv("LLMUX_POSTGRES", "postgres://from-the-environment/db")

	c, err := FromJSON([]byte(`{"postgres": ""}`))
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if c.Postgres != "" {
		t.Errorf("Postgres = %q; the document explicitly asked for no database and the "+
			"environment overruled it", c.Postgres)
	}
}

// A field the document is silent about is still filled from the (namespaced)
// environment. Document-wins is a precedence rule, not a switch that turns the
// environment off.
func TestLibraryEnvironmentStillFillsWhatTheDocumentOmits(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)
	t.Setenv("LLMUX_LOG_LEVEL", "debug")
	t.Setenv("LLMUX_MASTER_KEY", "sk-from-env")

	c, err := FromJSON([]byte(`{"server": {"addr": ":7000"}}`))
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want the env value: the document never mentioned log_level", c.LogLevel)
	}
	if c.Server.MasterKey != "sk-from-env" {
		t.Errorf("MasterKey = %q, want the env value: the document set server.addr but said "+
			"nothing about server.master_key, and presence is tracked per leaf, not per block",
			c.Server.MasterKey)
	}
	if c.Server.Addr != ":7000" {
		t.Errorf("Server.Addr = %q, want the document's value", c.Server.Addr)
	}
}

// The operator path must not have moved. `llmux serve` reads a file the
// operator wrote in a process the operator started; env-overrides-file is a
// deliberate deployment pattern there (one image, per-environment variables).
func TestOperatorModeKeepsTheHistoricalEnvLastOrder(t *testing.T) {
	clearKnownProviderEnv(t)
	clearLLMUXEnv(t)
	t.Setenv("DATABASE_URL", "postgres://shared")
	t.Setenv("LLMUX_ADDR", ":9999")

	path := writeTempConfig(t, &Config{
		Server:   ServerConfig{Addr: ":4000"},
		Postgres: "postgres://file",
	})
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Postgres != "postgres://shared" {
		t.Errorf("Postgres = %q, want the DATABASE_URL value — the sidecar's env-last "+
			"resolution is documented behaviour and was not supposed to change", c.Postgres)
	}
	if c.Server.Addr != ":9999" {
		t.Errorf("Server.Addr = %q, want the LLMUX_ADDR value", c.Server.Addr)
	}
}

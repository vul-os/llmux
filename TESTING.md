# Testing

llmux is tested in layers; CI (`.github/workflows/ci.yml`) runs all of them on
every push with Postgres + Redis service containers.

## Layers

1. **Unit tests** — pure logic, table-driven where natural. Every package.
   `make test` (race-enabled) or `go test ./...`.

2. **Translation/contract tests** — adapters' `convert.go` and the canonical
   `openai` types: request/response/stream/tool/vision mapping, finish-reason
   tables, round-trip JSON stability. These guard the OpenAI-compatibility seam.

3. **Integration tests** (gated) — real Postgres + Redis. Skipped unless env is
   set, so a plain `go test` never needs infra:
   ```
   export LLMUX_TEST_POSTGRES="postgres://user@localhost:5432/llmux_test?sslmode=disable"
   export LLMUX_TEST_REDIS="localhost:6379"
   go test ./core/keys/ ./core/cache/
   ```
   Cover the cross-replica properties: spend/budget persistence, Redis rate
   limiting, Redis cache.

   **These skip loudly.** When the env is absent, the package prints a banner
   naming each skipped test *and the properties nobody checked* — a skip count
   is not information. The gated corpus size is also read from the source and
   compared against what actually reached the gate, so a gated test that stops
   running is a failure rather than a quiet loss of coverage.

   **CI must not skip them.** The workflow sets `LLMUX_REQUIRE_INTEGRATION=1`,
   which turns any skip into a failure. The service containers are guaranteed
   there, so a skip means they did not come up — and a green run with zero
   integration coverage is worse than a red one.

4. **Conformance fixtures** (`core/conformance`) — record/replay transport.
   `make record` (needs real provider keys) captures real responses; replay runs
   them in CI so adapter translation is checked against *real* payloads.

5. **Live smoke** (gated on `LLMUX_LIVE=1` + provider keys) — `make smoke`.
   The gate that promotes a provider from `beta`/`experimental` to `stable`.

6. **Fuzz** — `FuzzMessageContentUnmarshal` (core/openai), `FuzzScanSSE`
   (core/provider). `go test -run Fuzz -fuzz=Fuzz... -fuzztime=30s ./...`.

7. **Structural guards** — tests that read the *source* instead of running it,
   so a property survives code that has not been written yet. They run in the
   normal `go test ./...` pass and each asserts a coverage floor, so a broken
   scan fails rather than passing vacuously:

   | Guard | Fails when |
   |---|---|
   | `TestNoUngatedProviderDispatch` (core/sovereign) | A call to an egress-capable provider method sits in a function that does not call `enforceSovereignty`. The egress-method set is read from the `Provider`/`Forwarder` interfaces, so a new verb is covered automatically. |
   | `TestOutboundDialSitesAreRegistered` (core/sovereign) | Any file gains the ability to open an outbound connection without being declared — with a written reason — in `outboundDialSites`. Also fails on *stale* entries. |
   | `TestServerPackageNeverDialsDirectly` (core/sovereign) | A `core/server` handler builds its own outbound HTTP request instead of dispatching through a (gated) provider adapter. |
   | `TestNoLoginEndpoints`, `TestNoCookiesIssued`, `TestServerReadsNoCredentialButBearer` (core/server) | An account/session/login surface appears: a login-shaped path answers `< 400`, a response sets a cookie, or `core/server` touches cookies at all. |
   | `TestAPIDocsCoverEveryServedRoute`, `TestAPIDocsDocumentNoPhantomRoutes` (core/server) | `docs/api.md` and the mux disagree: a served route is undocumented, a documented route 404s, or the credential the page names is rejected. Consumers write clients from that page without reading this package, so it has to stay exact. |
   | `TestSiteDocsInSync`, `TestSiteDocsHaveNoBrokenLinks`, `TestSiteDocsNavMatchesGenerator` (site/gen) | `site/docs` drifts from the canonical `docs/` tree, a generated page links to something the bundle does not ship, or the site nav and the generator disagree. Regenerate with `make site-docs`. |

   Each guard was verified by introducing the defect it targets and confirming
   it fails — a guard nobody has seen fail is a guard nobody has tested.

## Coverage

`make cover` (or `make cover-html` for a browsable report). With the integration
env set, current per-package coverage is high across config/providers/provider/
cache/router/openai/adapters; `cmd` and `passthrough` are lower because their
hot paths are exercised by *server* tests (cross-package execution isn't
attributed to the package under test).

## Conventions
- Tests must not require network for the default `go test ./...` run (gate with env + `t.Skip`).
- No production code changes to make a test pass — if a test reveals a bug, fix the bug or document current behavior and flag it.
- Every wave/feature ships with tests; provider adapters stay `experimental`/`beta` until live-verified (see SUPPORT.md).

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

4. **Conformance fixtures** (`core/conformance`) — record/replay transport.
   `make record` (needs real provider keys) captures real responses; replay runs
   them in CI so adapter translation is checked against *real* payloads.

5. **Live smoke** (gated on `LLMUX_LIVE=1` + provider keys) — `make smoke`.
   The gate that promotes a provider from `beta`/`experimental` to `stable`.

6. **Fuzz** — `FuzzMessageContentUnmarshal` (core/openai), `FuzzScanSSE`
   (core/provider). `go test -run Fuzz -fuzz=Fuzz... -fuzztime=30s ./...`.

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

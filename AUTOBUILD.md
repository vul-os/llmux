# Autonomous build log

Self-paced waves (15-min wakeups, ~4h budget, started 2026-06-20). Constraint:
**dependency-free** — the build/test environment may lack network for `go mod`
fetches, so no external Go modules (Postgres/Redis drivers, etc.). Use file-based
persistence as the production-swappable equivalent; the `keys.Store` and `Cache`
interfaces already allow a Postgres/Redis swap later.

Each wave must end with `go build ./...` + `go test ./...` green and `go vet` clean.

## Status
- [x] **Wave A — Pricing precedence.** `Source` interface; OpenRouter/LiteLLM/Azure/
  override(file+inline) sources; per-source catalog with **route-aware Cost**
  (override wins → routed-provider price → direct-best); disk cache (Save/Load,
  warm start); `GET /v1/catalog.json` open export. Verified live: catalog synced
  ~688 models from OpenRouter; 60+ tests green.
- [x] **Wave B — Persistence + admin.** `keys.FileStore` (JSON spend persistence,
  atomic writes, periodic flusher; `key_store_path` config) behind `keys.Store`;
  admin endpoints `GET /admin/keys` (masked + live spend) and `GET /admin/usage`
  (in-memory aggregate by key/model) guarded by master key. Verified at runtime;
  tests green.
- [x] **Wave C — Semantic cache.** `cache.SemanticCache` (cosine similarity over
  an `Embedder`, threshold/TTL/capacity) behind the `Cache` interface; server
  wires an in-process embedder calling its own embeddings route; `cache.semantic`
  + `embedding_model` + `similarity_threshold` config; canonical-text keying.
  Unit + end-to-end wiring tests green.
- [x] **Wave D — Cohere adapter** (partial). Full Cohere v2 chat adapter
  (`core/provider/cohere/`): request/response/stream/tools translation, wired
  into config + builder (`TypeCohere`, COHERE_API_KEY auto-detect). Tested vs
  synthetic Cohere payloads. NOTE: written to the documented v2 spec, unverified
  vs live API. Deferred: Gemini/Cohere embeddings.
- [x] **Wave F (tests) — Hardening tests.** Round-trip + fuzz tests for canonical
  types and SSE (`core/openai/roundtrip_test.go`, `core/provider/sse_extra_test.go`),
  fuzzers run clean (~1M+ execs).

- [x] **Wave E — CLI + structured logging.** `llmux serve|version|models|catalog|keys`
  subcommands (stdlib flag); slog structured access logs + `X-Request-ID`
  propagation via observeMW. Verified at runtime (models prints live 688-model
  pricing table).
- [x] **Wave D embeddings.** Gemini (embedContent/batchEmbedContents) + Cohere
  (/embed) embeddings implemented + tested; `/v1/embeddings` now served by them.
- [x] **Wave F — Bedrock adapter.** `core/provider/bedrock/` (Anthropic Claude via
  InvokeModel): pure-Go SigV4 signer verified against the AWS get-vanilla vector;
  non-stream invoke + synthesized streaming; wired (`TypeBedrock`, explicit
  config with region + AWS creds, no env auto-detect). Native eventstream framing
  is a follow-up. Unverified vs live service.

- [x] **Web UI (Vite + React).** Single Vite+React(JSX) app in `web/` (landing,
  docs, admin/usage dashboard via react-router) calling the gateway's
  /admin + /v1 endpoints. Built (`make web`) and embedded into the binary
  (`web/embed.go` go:embed) + served at `/ui` (SPA fallback, public; dashboard
  auths to /admin client-side). Mirrors LiteLLM (UI in OSS; SSO/RBAC/audit = ee/).
  Verified at runtime; tests green.

### Remaining backlog
- [ ] Native Bedrock event-stream (vnd.amazon.eventstream) streaming.
- [ ] Bedrock for non-Anthropic model families (Titan/Llama/Mistral on Bedrock).
- [ ] Postgres/Redis stores (needs deps — out of dep-free scope here).
- [ ] More docs/examples; thin native SDK sugar.

## Hardening phase (goal: real product / LiteLLM competitor)
Direction set 2026-06-20: breadth frozen; nothing promotes to "stable" without
live verification. See HARDENING.md + SUPPORT.md.
- [x] **H1 — honest stability tiers.** stable/beta/experimental per provider,
  surfaced in `/health`, warned at startup, documented in SUPPORT.md. (passthrough
  stable; anthropic/gemini beta; cohere/bedrock experimental — none live-verified yet.)
- [~] **H2 — conformance harness.** record/replay/live fixture Transport
  (`core/conformance`) proven (record→replay with network closed). Remaining:
  per-provider battery + recorded real fixtures + make record/smoke targets.
- [x] **H4 — production hardening.** Client-disconnect cancellation (tested),
  configurable upstream timeout, response-size bound (all providers),
  x-ratelimit-*/retry-after passthrough, error-taxonomy fidelity. See HARDENING.md.
- [x] **H5 — security review.** Adversarial agent pass + fixes: constant-time
  master-key compare; no URL/raw-body leakage in error responses; /metrics +
  /health gated; per-key cache isolation; Bedrock path escape; size bounds.
  SECURITY.md written. (A test that hung on httptest srv.Close() was caught by the
  600s timeout and fixed — verification doing its job.)
- [ ] H3 live smoke + record golden fixtures (BLOCKED on real keys).
- [ ] H6 distributed state Postgres/Redis (BLOCKED: needs running DB + deps to verify).

## Fixed
- `Message.Content` omitempty no-op: added custom `Message.MarshalJSON` that omits
  content when never set (preserves explicit null / string / parts).

## Notes
- Network was available this session (live OpenRouter sync worked; 688 models).
  Still keep the dep-free constraint — don't rely on `go mod` fetches succeeding.
- Final waves C/D/F-tests built via 3 parallel subagents (disjoint file sets:
  core/cache, core/provider/cohere, *_test.go), integrated + verified by main loop.

# llmux — Tasks

Tracks concrete work per phase (see `ROADMAP.md`). `[ ]` todo · `[~]` in progress
· `[x]` done. MVP = P1 + P2 + cost logging.

## Status — built in waves 0–6 (all Go tests pass, `-race` clean)

Implemented and tested: OpenAI-compat gateway (unary + byte-identical SSE),
passthrough + **Anthropic + Gemini** adapters (tools/vision/streaming both ways),
routing (alias / prefix / catch-all / fallback / **least-cost**), retries,
virtual keys (budgets / rate limits / allow-lists), exact LRU cache, **live price
catalog** (OpenRouter + LiteLLM sync, built-in offline seed) with cost in every
response, usage logging, Prometheus `/metrics`, `/v1/models`, `/v1/embeddings`,
Docker + Makefile. **Local wedge proven end-to-end in Python, Node, and Go.**

Deferred (interfaces ready, noted inline below): Postgres/Redis stores, semantic
cache, admin UI, per-OS/arch SDK wheels in CI, thin native SDK sugar.

---

---

## P0 — Project setup
- [ ] `go mod init github.com/<org>/llmux`; pick license split (core: Apache-2.0, ee: BSL-1.1)
- [ ] Repo layout per roadmap (`core/`, `ee/`, `cloud/`, `sdks/`, `docs/`)
- [ ] Config loader (env + YAML): providers, keys, routes, ports
- [ ] CI: build, test, lint, single static binary release; Dockerfile
- [ ] Decide canonical types: vendor OpenAI request/response Go structs as the internal contract

## P1 — Core gateway (every-language foundation)
- [ ] HTTP server skeleton + graceful shutdown + health endpoint
- [ ] `POST /v1/chat/completions` — non-streaming, OpenAI-exact request/response
- [ ] SSE streaming — wire-format byte-identical to OpenAI (`data:` chunks, `[DONE]`)
- [ ] Provider registry + interface (`Complete`, `Stream`, `Embed`)
- [ ] Tier-A pass-through adapter (route + swap key/base_url): OpenAI, Azure, DeepSeek, Groq, Together, Fireworks, xAI, OpenRouter, Ollama/vLLM
- [ ] Model-name → provider routing (alias map in config)
- [ ] **Verify "any language"**: hit the running gateway from `openai-python`, `openai-node`, AND `openai-go` with only `base_url` changed — all pass

## P2 — Adapters + normalization
- [ ] Anthropic adapter (prefer its OpenAI-compat endpoint; fall back to native translation)
- [ ] Gemini adapter (request/response + streaming translation)
- [ ] Tool / function-calling normalization across providers
- [ ] Vision / multimodal content-block normalization
- [ ] `POST /v1/embeddings` (unified)
- [ ] `GET /v1/models` (served from catalog, P4)
- [ ] Conformance tests: same request → equivalent normalized response across all adapters

## P3 — Gateway features
- [ ] Virtual keys (per-user/team), issue/revoke
- [ ] Budgets + spend caps per key; rate limits per key
- [ ] Fallback chains + retries with backoff
- [ ] Load balancing across keys/deployments of same model
- [ ] Routing strategies: alias, least-cost, least-latency
- [ ] Exact-match response cache
- [ ] Semantic cache (embedding similarity threshold)
- [ ] Routing/budget controls expressed via standard fields + `extra_headers`/`metadata` (no custom client needed)

## P4 — Pricing + usage
- [ ] Catalog schema (model, in/out cost, context window, max output, capability flags)
- [ ] Sync: OpenRouter `/models` (primary, live)
- [ ] Sync: merge LiteLLM open JSON for gaps (attribute, MIT)
- [ ] Sync: provider pricing APIs where exposed (Azure/Bedrock)
- [ ] Merge + manual-override layer; cron (hourly/daily)
- [ ] **Publish llmux catalog as open JSON** + serve via `/v1/models`
- [ ] Cost calculation; return cost in every response `usage` block
- [ ] Usage logging + export (per key/model/time)

## P5 — Ops + scale
- [ ] Postgres store (keys, budgets, logs); migrations
- [ ] Optional Redis (rate limits, cache, hot state)
- [ ] Stateless-core verification → horizontal scale behind LB
- [ ] Metrics (Prometheus) + structured logs + callbacks/webhooks
- [ ] Docker image + Helm chart + Fly/k8s deploy docs

## P6 — Polish + EE/Cloud
- [ ] Admin UI (keys, budgets, usage dashboards)
- [ ] Optional thin SDKs: `llmux-py`, `llmux-js`, `llmux-go` (ergonomics only)
- [ ] Docs site at `llmux.to` (quickstart per language, provider matrix, pricing)
- [ ] `ee/`: SSO/SAML, RBAC, audit log, multi-tenant admin
- [ ] `cloud/`: billing (flat fee + thin resale margin), committed-use discount pass-through

---

## Open questions / decisions
- [ ] Org/repo name on GitHub (`github.com/<org>/llmux`)
- [ ] EE license: BSL-1.1 vs commercial-only
- [ ] Semantic cache embedding model + store (pgvector vs Redis vector)
- [ ] First Cloud billing model: flat fee vs thin per-token resale (or both)

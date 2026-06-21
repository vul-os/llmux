# LiteLLM parity tracker

Grounded against the LiteLLM source (cloned, inventoried, deleted). LiteLLM's
surface is enormous — **~146 providers, hundreds of routes, ~60 logging
integrations, ~50 guardrails, ~60 DB tables**. Literal 1:1 parity is a long
program; this tracks it in priority tiers. **Functional parity** (the high-use
~80%) is the near-term goal. Verification gate still applies: adapters stay
`experimental` until live-verified (see SUPPORT.md).

Legend: ✅ have · 🟡 partial · ⬜ missing.

## Endpoints / modalities
| Endpoint | llmux | Notes |
|---|---|---|
| `/v1/chat/completions` (+stream) | ✅ | |
| `/v1/embeddings` | ✅ | passthrough, gemini, cohere |
| `/v1/models`, `/v1/catalog.json` | ✅ | catalog-backed |
| `/v1/completions` (legacy) | ✅ | via forwarder (passthrough) |
| `/v1/moderations` | ✅ | via forwarder |
| `/v1/images/generations` | ✅ | via forwarder |
| `/v1/audio/speech` (TTS) | ✅ | via forwarder |
| `/v1/rerank` | ✅ | via forwarder |
| `/v1/responses` | 🟡 | route forwards; lifecycle (get/cancel) TODO |
| `/v1/images/edits`, `/audio/transcriptions` (multipart) | ⬜ | Tier 1 (needs multipart model extraction) |
| Batches / Files / Fine-tuning / Assistants / Vector stores | ⬜ | Tier 2 (passthrough resources) |
| Realtime (WS), OCR, video, RAG, search | ⬜ | Tier 3 |
| Native provider passthrough (`/anthropic`,`/gemini`,`/bedrock`,…) | ⬜ | Tier 2 |
| MCP gateway | ⬜ | Tier 3 |

## Providers (~146 in LiteLLM; we have 5 adapter types)
- ✅ passthrough (covers OpenAI/Azure-AI/DeepSeek/Groq/Mistral/Together/Fireworks/xAI/OpenRouter/Ollama/vLLM/Perplexity/Cerebras/SambaNova/Nebius/Novita/DeepInfra… — most "openai_like" providers, cheaply)
- 🟡 anthropic, gemini (beta), cohere, bedrock (experimental)
- ⬜ Tier 1 native: Vertex AI (families), Azure OpenAI (full), Bedrock non-Anthropic families
- ⬜ Tier 2 native: HuggingFace, Replicate, Databricks, Watsonx, SageMaker, AI21, Voyage/Jina (embeddings/rerank), Deepgram/ElevenLabs/AssemblyAI (audio), Stability/BFL/Recraft/fal (images)
- ⬜ Tier 3: the long tail (~100 more)

## Routing / reliability
| Feature | llmux |
|---|---|
| alias / prefix / catch-all | ✅ |
| fallback chains, retries+backoff | ✅ |
| least-cost | ✅ |
| multiple deployments per model + weighted LB | ⬜ Tier 1 |
| latency-based / least-busy / usage-based (TPM) | ⬜ Tier 1 |
| cooldown / circuit-break failing deployments | ⬜ Tier 1 |
| context-window & content-policy fallback | ⬜ Tier 2 |
| tag-based routing | ⬜ Tier 2 |
| adaptive/complexity/quality/semantic auto-router | ⬜ Tier 3 |

## Keys / budgets / multi-tenancy
| Feature | llmux |
|---|---|
| virtual keys, per-key budget, RPM, model allow-list | ✅ |
| persistent spend | ✅ file **+ Postgres (cross-replica)** |
| TPM limits, parallel-request caps | ⬜ Tier 1 |
| teams, organizations, end-users | ⬜ Tier 1 |
| budget reset periods, key expiry/rotation | ⬜ Tier 1 |
| per-model budgets, tag spend | ⬜ Tier 2 |
| RBAC roles, SSO/SAML/JWT, SCIM, audit log | ⬜ Tier 2 (ee/) |

## Caching
✅ exact (LRU) + semantic (in-memory) + **Redis** (cross-replica). ⬜ dual / qdrant / s3 backends.

## Distributed state (H6 — done)
✅ **Postgres** key store (keys/spend/budget, cross-replica, auto-migrate, config-seed) · **Redis** rate limiter (fixed-window) + **Redis** response cache. Verified against local PG18 + Redis (integration tests + e2e runtime smoke).

## Observability (~60 in LiteLLM)
✅ JSONL usage, Prometheus, structured logs, /admin/usage. ⬜ pluggable callback system + OTEL, Langfuse, Datadog, S3/GCS, Slack alerting (Tier 1: hook system + 4–5 integrations).

## Guardrails (~50 in LiteLLM)
⬜ none yet. Tier 1: pre/post-call hook system + Presidio PII, banned-keywords, secret detection, Bedrock/OpenAI moderation, custom.

## Platform / ops
✅ metrics, CLI, embedded UI (read-mostly), Docker, request-id, cancellation, size limits, error taxonomy.
⬜ token counting (tiktoken-equiv), active per-model health checks, runtime model CRUD, alerting, secret managers, admin-UI depth (create-key/manage-models/charts/logs), Helm.

---

## Wave plan (priority-ordered; each gated by tests)
1. **W1 — Modalities via generic passthrough** (now): `/completions`, `/moderations`, `/images/*`, `/audio/*`, `/rerank` + a generic forwarder so passthrough providers serve OpenAI resource routes (Batches/Files/Fine-tuning/Assistants) cheaply. Testable with mocks.
2. **W2 — H6 distributed state** (Postgres + Redis) — unlocks multi-tenancy, persistent cache, cross-replica limits.
3. **W3 — Multi-tenancy**: teams/orgs/end-users, budget periods, key expiry, TPM + parallel caps. (on W2)
4. **W4 — Routing engine**: multiple deployments/model, weighted LB, latency/least-busy/usage strategies, cooldowns, active health checks.
5. **W5 — Observability hooks**: callback interface + OTEL/Langfuse/Datadog/Slack/S3.
6. **W6 — Guardrails**: pre/post hook system + Presidio/banned-keywords/secret-detection/provider moderation.
7. **W7 — Provider breadth**: Tier-1 natives (Vertex, Azure full, Bedrock families) then Tier-2; each via the fixture harness.
8. **W8 — Enterprise (ee/)**: SSO/SAML/JWT/RBAC/audit/SCIM; native passthrough routes; MCP; runtime model mgmt; admin-UI depth.

> Reality note: 146 providers / 60 integrations / 50 guardrails is a multi-month
> effort. We target functional parity (high-use subset) first and grow the long
> tail behind the verification gate.

## Correctness fixes from the LiteLLM diff (done)
Grounded against LiteLLM's implementations; fixed + tested:
- **Gemini**: real JSON-Schema sanitizer for tool params (was a no-op → 400s on
  pydantic/zod schemas); `functionResponse.name` recovery from `tool_call_id`;
  finish-reason `OTHER/SPII/IMAGE_*/LANGUAGE` → `content_filter`.
- **Anthropic**: `tool_choice:"none"` → explicit `{"type":"none"}`; `stop_reason:"refusal"` → `content_filter`.
- **Bedrock**: model-id full percent-encoding for inference-profile/ARN ids; hard-gate non-Anthropic model ids with a clear 400 (adapter is Claude-only until Converse).
- **Pricing**: **cached-token pricing** (`cache_read` rate parsed from LiteLLM, `prompt_tokens_details.cached_tokens` honored in `Cost` — was overcharging cache-heavy workloads up to ~10×); corrected stale DeepSeek seed (0.28/0.42).

## Round 2 — audit-driven fixes (done)
Grounded against LiteLLM; from a 3-axis audit (parity / testing / efficiency):
- **Adapters now map** `response_format` (Gemini native `responseMimeType`/`responseSchema`; Anthropic JSON-instruction fallback), `parallel_tool_calls` → Anthropic `disable_parallel_tool_use`, and `user` → Anthropic `metadata.user_id` (were silently dropped).
- **Error normalization**: all adapters set OpenAI-canonical error `type` via `NormalizeErrorType(status)` (Gemini no longer leaks `RESOURCE_EXHAUSTED`; Cohere no longer hardcodes `upstream_error`) + `context_length_exceeded` code heuristic.
- **Confirmed bugs fixed**: Gemini streaming empty tool-args (`""`→`{}`); Bedrock data-URI mislabeled base64; Bedrock error parsing (`Message`/`X-Amzn-ErrorType`).
- **Rate-limit/`retry-after` headers** now relayed on **error** responses (429/5xx), all providers + server.
- **`/v1/models`** emits a real `created` timestamp.
- **Efficiency**: single-pass body rewrite (was triple-parsing per request); micro-dollar money (no float drift); metric/usage **cardinality bounded** (DoS); slice preallocation in adapters.

## Round 3 — "perfection" backlog (done)
- **Router prefix-wildcards** — `claude-*`, `gpt-4*` patterns; precedence exact > longest-prefix > `*`; LiteLLM-style target substitution.
- **`drop_params`** — config list of request fields stripped before forwarding to OpenAI-shaped upstreams (single chokepoint in the body rewriter).
- **Azure OpenAI** as a request provider — dedicated adapter (`api-key` header, `api-version` query, deployment-path URLs; chat/stream/embeddings/forward).
- **Token-array embeddings** — Gemini/Cohere now return a clear, actionable error (route token arrays to passthrough).
- **Efficiency**: lock-free keys store (atomic spend, per-bucket mutex, no global lock); catalog `atomic.Pointer` snapshot (lock-free reads, no reader stall on sync); SigV4 **signing-key cache**; cache stores **pre-serialized bytes** (no re-marshal / shared-pointer on hit). All with `-race` concurrency tests.

### Still deferred (lower-frequency / needs live keys)
- Bedrock **Converse API** + native eventstream (unlocks Nova/Llama/Mistral/Titan + true streaming).
- Cohere v2 streaming tool-call shape + `billed_units` usage — needs **live verification** (experimental).
- `cache_control` passthrough; reasoning/thinking token surfacing.
- Pricing dims: tiered (above-128k), audio, reasoning-rate, batch, service-tier, regional uplift.

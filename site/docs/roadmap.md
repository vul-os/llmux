# llmux — Roadmap

> The LLM multiplexer. One gateway, every provider, **every language**.
> OSS core (self-host free, forever) + `ee/` for hosted **llmux Cloud**.

For the full day-to-day task/phase breakdown see [`tasks.md`](tasks.md); for the
provider/feature parity matrix against LiteLLM see [`PARITY.md`](PARITY.md).
This file tracks direction at a glance.

---

## The core idea

A single Go binary that speaks the OpenAI-compatible HTTP API (REST + SSE).
Every language already ships a mature OpenAI client that accepts a custom
`base_url` — point it at llmux and get routing, fallbacks, budgets, caching,
live cost, and (by default) on-box inference underneath, with zero
per-language code.

```
llmux/
  core/         # MIT — gateway, providers, routing, sovereignty gate, BYO-keys, pricing sync
    server/     # HTTP server: /v1/chat/completions, /v1/embeddings, /v1/models, ...
    provider/   # adapters: passthrough, anthropic, gemini, bedrock, cohere
    sovereign/  # default-deny egress classifier + tier policy
    keys/       # virtual keys, budgets, rate limits
    cache/      # exact + semantic response cache
  integration/cp/ # optional control-plane billing seam (spool + reconcile)
  web/          # embedded admin dashboard (React, served via go:embed at /ui)
  docs/         # user guide, admin/ops, troubleshooting, architecture
  ee/, cloud/   # planned: hosted llmux Cloud (SSO/RBAC/audit, multi-tenant)
```

- **Stateless core** → scale horizontally behind a load balancer.
- **State** (keys, budgets, cache) in Postgres + optional Redis; self-host
  defaults to in-memory/embedded, no external DB required.
- `core/` stays free and self-hostable forever; `ee/`/`cloud/` are not yet built.

---

## Now — shipped and in daily use

- OpenAI-compatible gateway: `chat/completions` (unary + byte-identical SSE),
  `embeddings`, `models`, `completions`, `moderations`, `images/generations`,
  `audio/speech`, `audio/transcriptions`, `audio/translations`, `responses`,
  `rerank`.
- Native adapters for Anthropic and Gemini (tools/vision/streaming both ways);
  passthrough for any OpenAI-shaped upstream (OpenAI, Azure, DeepSeek, Groq,
  Together, Fireworks, xAI, OpenRouter, Ollama/vLLM, and more); Cohere and
  Bedrock adapters are experimental.
- Routing: alias/prefix/wildcard/catch-all, fallback chains with retries, and
  least-cost selection.
- **Sovereignty gate**: default-deny egress before every dispatch path (chat,
  streaming, embeddings, semantic-cache embedder, all modality/forward
  routes). A 4-tier model — local / sovereign / brokered / external — governs
  what's allowed by default vs. requires an explicit opt-in; posture is
  disclosed at `/health` and in the startup log.
  See [`docs/architecture.md`](docs/architecture.md#the-sovereignty-gate-where-your-ai-runs).
- Virtual keys with per-key USD budgets, RPM limits, and model allow-lists;
  spend hashed at rest in Postgres, rate limits in Redis.
  Fail-closed behavior throughout: keyless non-loopback binds refused,
  unpriced-model spend denied rather than allowed, Postgres/Redis outages on
  key lookup deny rather than allow.
- Exact-match (LRU/TTL) and semantic (embedding-similarity) response caching,
  scoped per key, shared via Redis; cache hits are never billed.
- Live price catalog synced from OpenRouter + LiteLLM, with a built-in offline
  seed; cost returned in every response's `usage` block.
- Optional control-plane billing seam (`integration/cp`) with durable on-disk
  usage spooling and reconciliation, invisible when `LLMUX_CP_URL` is unset.
- Optional shared Postgres via `DATABASE_URL`/`VULOS_DATABASE_URL` under a
  dedicated `llmux` schema, so llmux can share one database with the rest of
  the suite.
- Embedded admin dashboard at `/ui` (usage, keys, live catalog, docs) —
  no separate service; Prometheus `/metrics`, structured logs, `/health`.
- Comprehensive docs: quickstart, API reference, configuration, architecture,
  admin guide, troubleshooting, control-plane seam.

## Next

- Multipart `images/edits`; `/v1/responses` lifecycle (get/cancel).
- Native passthrough for provider-native routes (`/anthropic`, `/gemini`,
  `/bedrock`, …) alongside the OpenAI-compat surface.
- Tier-1 provider gaps: full Vertex AI families, full Azure OpenAI, Bedrock
  non-Anthropic model families.
- Multiple deployments per model with weighted/latency-based/least-busy load
  balancing and cooldown/circuit-breaking of failing deployments.
- Teams/organizations/end-users, budget reset periods, key expiry/rotation,
  TPM limits and parallel-request caps.
- Thin native SDKs (`llmux-py`, `llmux-js`, `llmux-go`) — ergonomics sugar
  over the HTTP contract, never required.

## Later

- `ee/`: SSO/SAML, RBAC, SCIM, audit log, multi-tenant admin.
- `cloud/`: hosted llmux Cloud — flat platform fee and/or thin per-token
  resale margin, committed-use discount pass-through.
- Long-tail provider coverage (HuggingFace, Replicate, Databricks, Watsonx,
  SageMaker, AI21, Voyage/Jina, Deepgram/ElevenLabs/AssemblyAI, image
  providers) and Tier-3 surfaces (Batches/Files/Fine-tuning/Assistants/Vector
  stores, Realtime WS, MCP gateway, adaptive/semantic auto-routing).

---

## Non-negotiables

- OpenAI HTTP compatibility is sacred — never break the client contract.
- `core/` stays free and self-hostable forever.
- No provider detail leaks past the gateway boundary.
- Inference stays on-box by default; nothing egresses silently (the
  sovereignty gate is not optional).
- Pricing catalog stays open and auto-synced.

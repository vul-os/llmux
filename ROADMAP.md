# llmux — Roadmap

> The LLM multiplexer. One gateway, every provider, **every language**.
> OSS core (self-host free, forever) + `ee/` for hosted **llmux Cloud**.

For the provider/feature parity matrix against LiteLLM see
[`docs/parity.md`](docs/parity.md). This file tracks direction at a glance;
[`CHANGELOG.md`](CHANGELOG.md) records what has actually shipped.

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
  web/          # embedded admin dashboard (one hand-written ui.html, go:embed at /ui)
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
- Embedded admin dashboard at `/ui` (usage, keys, live catalog) — a single
  hand-written HTML page, no framework and no build step; no separate
  service; Prometheus `/metrics`, structured logs, `/health`.
- Comprehensive docs: quickstart, API reference, configuration, architecture,
  admin guide, troubleshooting, control-plane seam.

## Next

- Installable dashboard: a web manifest and icons for `/ui` so an operator
  can add it to a phone home screen or run it as its own desktop-style
  window — small, mostly-assets work. Offline support (service worker,
  cache invalidation, update/staleness handling) is a separate and
  materially bigger job, not planned alongside it: `/ui` exists to show
  live spend, keys and catalog state, and a cached snapshot of that has
  limited value once it can no longer reach the gateway.
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

### Publishing the language packages

Nothing is published to any registry today, and that is a decision, not an
oversight. The coordinates below are written into the manifests and reserved by
intent only — **no account holds them**, so any of them can be taken by someone
else tomorrow. That is not hypothetical: plain `llmux` was lost on both PyPI and
crates.io to unrelated projects before anyone looked, and the crates.io one is a
same-category tool at 2.4.0.

| registry | coordinate |
|---|---|
| npm | `@vul-os/llmux`, `@vul-os/llmux-bun` |
| JSR | `@vul-os/llmux` |
| PyPI | `vul-os-llmux` |
| crates.io | `vul-os-llmux` |
| RubyGems | `vul-os-llmux` |
| NuGet | `VulOs.Llmux` |
| Packagist | `vul-os/llmux` |
| Hex | `vul_os_llmux` (Hex forbids hyphens) |
| Maven | `org.vulos:llmux` (a groupId is reverse-DNS of a domain; `vul-os` is a GitHub handle) |
| Go | `github.com/vul-os/llmux` — no registry, the module path is the coordinate |

Checked free across npm, JSR, PyPI, crates.io, RubyGems, NuGet, Packagist and
Hex on 2026-08-10, before being written down.

**Before anything is pushed**

1. **Claim the scopes first, publish second.** `@vul-os` has to exist as an
   organisation on npm and on JSR before a scoped package can go anywhere, and
   claiming a scope is free and reversible in a way that losing a name is not.
   NuGet ID-prefix reservation for `VulOs.*` is optional but the same logic.
2. **A release has to produce the artifacts.** Today the release workflow builds
   the binary and the C ABI bundles; it does not build a wheel, an npm tarball,
   a gem or a nupkg. Publishing without that step is publishing whatever happens
   to be in a working tree.
3. **Each package must install from a clean checkout.** This was false until
   recently and silently so — `pip install -e sdks/python` failed on every clone
   (hatchling force-includes `llmux/bin`, which is gitignored, and raises `FileNotFoundError` when it is absent), and `npm pack` produced a tarball containing only a README and a
   package.json. Both are fixed and verified against `git archive HEAD`; the
   others are not all verified.
4. **Decide `llmux.to`.** Several manifests carry it as the homepage. A domain
   question, not a registry one, but it ships in package metadata.

**When a package does go out**, delete its registry from `UNPUBLISHED` in
`scripts/check-sdk-versions.mjs`. That list exists to refuse documentation that
tells a reader to install something that is not there; it is meant to shrink,
and the entry is what keeps the docs honest until it does.

**Order.** Go needs no registry at all — a module path is the coordinate, so it
is already "published" by tagging. Of the rest, the ones whose artifacts are
verified installable are the only candidates for a first push.


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

# llmux Enterprise (`ee/`)

This directory holds enterprise/cloud features that power **llmux Cloud**. The
entire project — including `ee/` — is **MIT licensed** (like LiteLLM). The
business model is the **hosted service**, not a restrictive code license:
self-hosting everything is free forever.

- **`core/`** — the gateway. MIT, self-hostable. You pay only your own infra +
  provider tokens (BYO keys).
- **`ee/`** — SSO/SAML, RBAC, audit logging, multi-tenant admin, advanced
  analytics. Used by llmux Cloud; MIT.
- **`cloud/`** — the hosted service (uses `core/` + `ee/`).

## Why self-hosting stays free at scale

A single static Go binary, stateless core, BYO keys. Marginal cost ≈ CPU; no
per-request fee. Scale horizontally behind a load balancer with state (keys,
budgets, usage, cache) in Postgres + optional Redis. The `keys.Store` and cache
interfaces in `core/` are already swappable for those backends.

## How llmux Cloud undercuts OpenRouter

OpenRouter takes ~5–5.5% on every token. llmux Cloud can charge less:
1. **Flat platform fee** (or free on OSS) instead of a per-token cut.
2. **Thinner managed margin** (~2–3%) when reselling tokens.
3. **Volume/committed-use discounts** aggregated across Cloud, partly passed through.
4. **Exact + semantic caching** cuts real provider calls → effective price below
   OpenRouter, which doesn't cache your traffic.

The open, auto-synced price catalog makes the savings visible per model.

> No code here yet — this is the placeholder + license boundary for the open-core
> split so the structure is correct from day one.

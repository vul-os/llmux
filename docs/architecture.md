# Architecture

llmux is a single Go binary. The **OpenAI HTTP schema is the canonical
contract**: providers are adapters behind it, routing and budget controls ride on
standard request fields plus `extra_headers` / `metadata`, and the streaming
format is byte-identical to OpenAI so every language's stream parser just works.

```text
core/                the open gateway (no cp dependency)
  openai/            canonical OpenAI wire types (the contract)
  server/            HTTP gateway, streaming, auth, metrics, usage
  sovereign/         the sovereignty gate (where your AI runs; default-deny egress)
  provider/          Provider interface + SSE utilities
    passthrough/     OpenAI-shaped upstreams
    anthropic/ gemini/ cohere/ bedrock/ azure/   native adapters
  router/            routing + least-cost selection
  keys/              virtual keys, budgets, rate limits (Postgres + Redis)
  byok/              per-account provider keys, AES-256-GCM under an operator KEK
  cache/             exact + semantic response cache
  pricing/           catalog + live sync + cost accounting
  config/            JSON config loader
cmd/llmux/           the binary (server + CLI subcommands)
integration/cp/      OPTIONAL control-plane (billing/entitlements) adapter
web/                 Vite + React admin SPA, embedded at /ui
```

## The sovereignty gate (where your AI runs)

llmux is Vulos's **sovereign** LLM gateway: inference runs on **your** box by
default, and a request is **never silently sent to a company that mines you**.
This is enforced, not documented-hope. `core/sovereign` classifies every
configured provider by *where its traffic goes* and the server calls the gate
(`core/server/sovereignty.go`, `enforceSovereignty`) **before any network call
on every dispatch path** — chat, streaming chat, embeddings, the semantic-cache
embedder, and all model-bearing modality routes (`/v1/completions`,
`/v1/responses`, images, audio, moderations, rerank).

Providers resolve to a 4-tier dial, most→least private:

| Tier | What it is | Default |
|---|---|---|
| **local** | inference on THIS box (loopback / unix socket) | always allowed |
| **sovereign** | an operator-declared endpoint the operator vouches for (unverified by Vulos) | allowed on the operator's declaration |
| **brokered** | a named third party under a claimed no-train agreement | blocked until `allow_brokered` |
| **external** | any other off-box endpoint (may mine/train) | **blocked** until `allow_egress` |

The gate **fails closed**: an empty/unparseable base URL, an off-box endpoint
marked `local`, or any unrecognized tier is treated as **external and blocked**.
Nothing silently upgrades — `sovereign`/`brokered` are explicit operator config
declarations; an unmarked off-box endpoint derives `external` from its locality.
A blocked provider never opens a socket; the denial is logged and counted
(`egress_blocked` metric), and every *permitted* off-box call is logged with its
tier so egress is always observable, never silent. On failover, a blocked
primary is skipped so a local fallback can still serve; if every target is
blocked the 403 surfaces. The `/health` endpoint (master-key only) discloses the
full posture: each provider's tier, label, and whether it may egress.

Operators opt a provider in per-provider in the config (never globally):
`"tier": "sovereign"`, `"allow_brokered": true`, or `"allow_egress": true`.

## The canonical contract

Every provider implements one `Provider` interface and speaks only the canonical
OpenAI types. Provider-specific quirks — Anthropic's content blocks, Gemini's
schema, Bedrock's signing — stay behind that seam. This is what lets any OpenAI
SDK work unchanged regardless of which provider ultimately serves the request.

## The control-plane is isolated

The `core` packages **never import** `integration/cp`. The optional
control-plane adapter is wired only by `cmd/llmux`, and only when `LLMUX_CP_URL`
is set. Delete `integration/cp` entirely and the standalone build still compiles
and runs. See the [control-plane seam](control-plane.md) for details.

## Where llmux sits in Vulos

llmux is the **LLM access layer** for the Vulos suite. In a Vulos deployment the
**box is the authority** (it holds your data and runs your sovereign services),
**relay** is the single reachability ingress, and **cloud** is a content-blind
control plane (billing/entitlements only). llmux runs as one of the box's
sovereign services: its default-local sovereignty gate is exactly the box-as-
authority principle applied to inference. The optional `integration/cp` adapter
is how the box reports metered usage to the cloud control plane — it never sends
prompts or completions there, only billing counts.

Standalone (no cp, no Vulos suite) llmux is a complete self-hosted gateway on
its own; the same binary and code path serve both self-host and managed.

## Related

- [Providers](../web/docs/providers.md) — native adapters vs. passthrough
- [Routing & reliability](../web/docs/routing.md) — how a model name resolves to a provider
- [Control-plane seam](control-plane.md) — the optional cloud billing adapter
- [LLM access: BYOK vs central](LLM-ACCESS.md) — per-account key resolution and metering

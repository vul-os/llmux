# Architecture

llmux is a single Go binary. The **OpenAI HTTP schema is the canonical
contract**: providers are adapters behind it, routing and budget controls ride on
standard request fields plus `extra_headers` / `metadata`, and the streaming
format is byte-identical to OpenAI so every language's stream parser just works.

```text
core/                the open gateway (no cp dependency)
  openai/            canonical OpenAI wire types (the contract)
  server/            HTTP gateway, streaming, auth, metrics, usage
  provider/          Provider interface + SSE utilities
    passthrough/     OpenAI-shaped upstreams
    anthropic/ gemini/ cohere/ bedrock/ azure/   native adapters
  router/            routing + least-cost selection
  keys/              virtual keys, budgets, rate limits (Postgres + Redis)
  cache/             exact + semantic response cache
  pricing/           catalog + live sync + cost accounting
  config/            JSON config loader
cmd/llmux/           the binary (server + CLI subcommands)
integration/cp/      OPTIONAL control-plane (billing/entitlements) adapter
web/                 Vite + React admin SPA, embedded at /ui
```

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

## Related

- [Providers](../web/docs/providers.md) — native adapters vs. passthrough
- [Routing & reliability](../web/docs/routing.md) — how a model name resolves to a provider

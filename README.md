# llmux

**One OpenAI-compatible endpoint for every LLM provider.**

[![License: MIT](https://img.shields.io/badge/License-MIT-0f6a6c.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![React](https://img.shields.io/badge/React-18-61DAFB?logo=react&logoColor=black)](https://react.dev)
[![Tests](https://img.shields.io/badge/tests-passing-4f7a4d)](TESTING.md)

llmux is a single Go binary that speaks the **OpenAI HTTP API** and routes every request to the provider behind it — OpenAI, Anthropic, Azure OpenAI, AWS Bedrock, Cohere, Gemini, or any OpenAI-shaped upstream via passthrough. Because every language already ships a mature OpenAI client that accepts a custom `base_url`, you point your existing SDK at llmux and get **routing, fallbacks, per-key budgets, caching, and live cost** underneath — with zero per-language code.

It is self-hosted, open source, has no telemetry, and ships its admin dashboard *inside* the binary. It runs fully standalone; an optional control-plane seam adds centralized billing and entitlements when you want them.

```text
  any OpenAI client ──(base_url = llmux)──▶  ┌─────────┐ ──▶ OpenAI · Azure
                                             │         │ ──▶ Anthropic
                                             │  llmux  │ ──▶ Gemini · Cohere · Bedrock
                                             │   mux   │ ──▶ DeepSeek · Groq · OpenRouter …
                                             └─────────┘ ──▶ 100+ via passthrough
```

---

## Features

- **OpenAI-compatible `/v1/*` API** — `chat/completions`, `completions`, `embeddings`, `models`, plus `responses`, `rerank`, `moderations`, `images/generations`, and `audio/speech`. Works with any OpenAI SDK unchanged.
- **Multi-provider routing** — native adapters for **Anthropic, Gemini, Cohere, AWS Bedrock, and Azure OpenAI**, plus **passthrough** for any OpenAI-shaped upstream (OpenAI, DeepSeek, Groq, Mistral, Together, OpenRouter, Ollama/vLLM, …). Tool-calling, vision, and streaming are translated per provider.
- **Flexible routing** — model aliases, `provider/model` prefixes, prefix wildcards (`claude-*`), a catch-all route, **fallback chains** with retries/backoff, and **least-cost** candidate selection.
- **Byte-identical SSE streaming** — streamed responses match OpenAI's wire format, so every language's stream parser just works.
- **Caching** — exact-match (LRU + TTL) and **semantic** (embedding-similarity) response caching, in-memory or shared across replicas via Redis. Cache entries are scoped per virtual key.
- **Virtual keys & budgets** — per-key USD budgets, RPM rate limits, and model allow-lists. Spend persists in Postgres; rate limits live in Redis.
- **Live pricing catalog** — a seed ships built-in (cost works offline) and auto-syncs from OpenRouter + LiteLLM, merged by precedence with manual overrides. Cost appears in each response's `usage` block; the merged catalog is republished at `GET /v1/catalog.json`.
- **Embedded admin dashboard** — usage by model, virtual-key budgets, and the live price catalog, served from the binary at `/ui` via `go:embed`. No separate service.
- **Hardened by default** — constant-time master-key auth, response size limits, request-body caps, upstream timeouts/cancellation, OpenAI-canonical error normalization, and `drop_params`. Prometheus `/metrics`, structured logs, and `/health`.
- **Optional control-plane billing seam** — wire in a Vulos-style control plane (`cp`) to resolve identity, gate budget, and report usage centrally. Entirely opt-in and isolated from the core.

---

## Architecture

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

The OpenAI HTTP schema is the canonical contract: providers are adapters behind it, routing and budget controls ride on standard fields plus `extra_headers` / `metadata`, and the streaming format is byte-identical to OpenAI. The `core` packages never import `integration/cp`; the control-plane adapter is wired only by `cmd/llmux` and only when `LLMUX_CP_URL` is set — delete it and the standalone build still works.

---

## Quick start

**Prerequisites:** Go 1.25+, Node 18+ (only to rebuild the embedded web UI), and at least one provider API key.

```bash
# 1. Build the binary (embeds the prebuilt web UI from web/dist)
make build

# 2. Configure providers (env vars are referenced by the config)
export OPENAI_API_KEY=...
export ANTHROPIC_API_KEY=...

# 3. Run — gateway on :4000, dashboard at /ui
cp llmux.example.json llmux.json
./dist/llmux -config llmux.json
```

The config path also reads from `LLMUX_CONFIG`. See [`llmux.example.json`](llmux.example.json) for a full, commented example covering providers, routes, fallbacks, least-cost candidates, caching, keys, and the pricing catalog.

### Configuration

Most settings live in the JSON config; common ones are also overridable by env var:

| Env var | Purpose |
|---|---|
| `LLMUX_CONFIG` | Path to the JSON config file |
| `LLMUX_ADDR` | Listen address (default `:4000`) |
| `LLMUX_MASTER_KEY` | Admin/master key for `/admin`, `/metrics` |
| `LLMUX_POSTGRES` | Postgres DSN (virtual keys + spend) |
| `LLMUX_REDIS` | Redis address (rate limits + shared cache) |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, … | Provider credentials (referenced by `api_key_env` in config) |
| `LLMUX_CP_URL`, `LLMUX_CP_SECRET` | Optional control-plane URL + shared secret (see below) |
| `LLMUX_LOG_LEVEL` | Log verbosity |

Postgres and Redis are optional — for single-replica use, keys and cache work in-memory. Set them for multi-replica correctness.

---

## Usage

Point any OpenAI client at llmux — the model string selects the route:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-team-a" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:4000/v1", api_key="sk-team-a")

resp = client.chat.completions.create(
    model="cheapest",                       # least-cost route from your config
    messages=[{"role": "user", "content": "hi"}],
)
print(resp.usage)                           # includes per-request cost
```

Add `"stream": true` for byte-identical OpenAI SSE. List available models and their pricing with `GET /v1/models`, or from the CLI:

```bash
./dist/llmux models       # models with pricing + context window
./dist/llmux catalog      # price catalog count and last sync time
./dist/llmux keys         # virtual keys: budget, spend, rpm
```

---

## Optional: control-plane billing seam

For standalone use, leave it off — llmux uses static virtual keys from your config. To centralize identity, budget, and usage reporting across a fleet, set `LLMUX_CP_URL` / `LLMUX_CP_SECRET` (or the `cp` block in config). The gateway then resolves identity, gates budget, and reports usage to the control plane over an `X-Relay-Auth` shared secret. If the control plane is unreachable, llmux defaults to a conservative per-account rate cap (configurable via `cp_degraded_rpm`, or fail-open via `cp_degraded_fail_open` if you accept the spend risk).

---

## Development

```bash
make web        # rebuild the embedded React/Vite admin UI into web/dist
make build      # build the gateway binary (embeds web/dist)
make run        # build and run on :4000
make docker     # build the Docker image
```

The web app uses `.jsx` only (never `.tsx`).

### Testing

```bash
make test       # all Go tests with -race
make vet        # static analysis
make cover      # coverage summary
```

Integration tests against Postgres/Redis activate when `LLMUX_TEST_POSTGRES` / `LLMUX_TEST_REDIS` are set. Provider conformance fixtures and the live smoke suite (`make record`, `make smoke`) require real provider keys. See [TESTING.md](TESTING.md).

---

## Self-hosting

llmux is a single binary with no required runtime dependencies — drop it on a host (or use the included [`Dockerfile`](Dockerfile)), point it at a config, and set your provider keys. Add Postgres and Redis when you scale to multiple replicas so keys, spend, rate limits, and cache stay consistent. See [HARDENING.md](HARDENING.md) for production posture.

---

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md). Do not file public issues for security problems.

## License

[MIT](LICENSE) — free to use, modify, and distribute.

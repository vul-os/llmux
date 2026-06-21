<p align="center">
  <img src="assets/logo.svg" width="380" alt="llmux — the LLM multiplexer" />
</p>

<h3 align="center">One OpenAI-compatible gateway for every provider, in every language.</h3>

<p align="center">
  A single Go binary that speaks the OpenAI API and routes to any LLM behind it —<br/>
  routing, fallbacks, budgets, caching, and live cost, with zero per-language code.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/license-MIT-4fe3c8?style=flat-square" alt="MIT" />
  <img src="https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.25" />
  <img src="https://img.shields.io/badge/tests-349%20passing-ff9a4d?style=flat-square" alt="tests" />
  <img src="https://img.shields.io/badge/%2Drace-clean-4fe3c8?style=flat-square" alt="race clean" />
  <img src="https://img.shields.io/badge/binary-single%20static-ff9a4d?style=flat-square" alt="single binary" />
</p>

<p align="center">
  <a href="#quickstart"><b>Quickstart</b></a> ·
  <a href="#features">Features</a> ·
  <a href="#where-llmux-fits">Comparison</a> ·
  <a href="SUPPORT.md">Providers</a> ·
  <a href="PARITY.md">Parity</a> ·
  <a href="https://llmux.to">Cloud</a>
</p>

<p align="center">
  <img src="assets/landing.jpg" width="880" alt="llmux landing" />
</p>

---

llmux works in **every language on day one with zero per-language code**. Every
ecosystem already ships a mature OpenAI client that accepts a custom `base_url` —
point it at llmux and you get a control plane underneath: provider routing,
fallback chains, per-key budgets, response caching, and real cost in every
response.

```text
  any-language app ──(OpenAI SDK, base_url = llmux)──▶  ┌─────────┐ ──▶ OpenAI
                                                        │         │ ──▶ Anthropic
                                                        │  llmux  │ ──▶ Gemini · Cohere · Bedrock · Azure
                                                        │   mux   │ ──▶ DeepSeek · Groq · xAI · Mistral …
                                                        └─────────┘ ──▶ 100+ via passthrough
```

---

## Quickstart

```bash
make build
export OPENAI_API_KEY=...        # providers are auto-detected from env
export ANTHROPIC_API_KEY=...
./dist/llmux                     # gateway on :4000  (dashboard at /ui)
```

Point **any** OpenAI client at it — the model string selects the provider:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:4000/v1", api_key="x")

client.chat.completions.create(
    model="anthropic/claude-3-5-sonnet",   # any provider, one client
    messages=[{"role": "user", "content": "hi"}],
)
```

### Or embed it locally — no server to run

Each language package bundles the binary and starts it as a local sidecar (Go
runs it in-process). Integrate in any language without standing up a server:

```python
import llmux
client = llmux.OpenAI()                     # spawns the gateway, returns an OpenAI client
client.chat.completions.create(model="gemini-1.5-pro", messages=[...])
```

```js
const llmux = require("llmux");
const client = await llmux.OpenAI();
await client.chat.completions.create({ model: "gpt-4o", messages: [...] });
```

```go
local, _ := llmux.Start(llmux.Options{})    // in-process, no subprocess
defer local.Close()
// point any OpenAI Go client at local.OpenAIBaseURL()
```

See [`sdks/`](sdks/) for details.

---

## Built-in dashboard & live docs

A web dashboard **and** rendered docs ship **inside the binary** at `/ui` — no
separate Node server at runtime. Usage by model, virtual-key budgets, and the
live price catalog, embedded via `go:embed`.

<p align="center">
  <img src="assets/dashboard.jpg" width="49%" alt="llmux dashboard" />
  <img src="assets/docs.jpg" width="49%" alt="llmux docs" />
</p>

---

## Features

| Area | What you get |
|------|--------------|
| **Any language** | OpenAI-compatible REST + **byte-identical** SSE; works with every OpenAI SDK unchanged |
| **Providers** | Passthrough (OpenAI, DeepSeek, Groq, Mistral, Together, Fireworks, xAI, OpenRouter, Ollama/vLLM) + native adapters (Anthropic, Gemini, Cohere, AWS Bedrock, Azure OpenAI) with tool-calling, vision & streaming translation |
| **Routing** | Aliases, `provider/model` prefix, **prefix wildcards** (`claude-*`), catch-all, fallback chains, retries, and **least-cost** selection |
| **Live cost** | Price catalog auto-synced from OpenRouter + LiteLLM; **micro-dollar** accounting; cost in every response `usage` block; `/v1/models` from the catalog |
| **Governance** | Virtual keys with per-key budgets, rate limits, and model allow-lists; spend in Postgres, limits in Redis |
| **Caching** | Exact-match (LRU + TTL) **and** semantic (embedding-similarity); in-memory or shared via Redis |
| **Hardening** | Cancellation, upstream timeouts, body limits, rate-limit header relay, OpenAI-canonical error normalization, `drop_params` |
| **Ops** | Prometheus `/metrics`, structured logs + `X-Request-ID`, JSONL usage log, admin endpoints, health check, single static binary, Docker |
| **Dashboard** | Vite + React app (landing · docs · admin) embedded in the binary at `/ui` |

---

## Where llmux fits

Honest positioning. llmux is best-in-class for **self-hosted, single-binary**
deployments and is engineered for correctness — but it is younger than the
incumbents on breadth and battle-testing, and we say so.

| Capability | llmux | LiteLLM | OpenRouter |
|---|:---:|:---:|:---:|
| Single binary, no runtime | ✅ one Go binary | ❌ Python app | — hosted SaaS |
| Drop-in OpenAI API, any language | ✅ | ✅ proxy | ✅ API |
| Self-host, bring your own keys | ✅ | ✅ | ❌ |
| Routing + fallback + least-cost | ✅ | ✅ | ◑ auto only |
| Exact + semantic caching | ✅ | ✅ | ❌ |
| Live cost in every response | ✅ | ◑ | ✅ |
| Provider breadth | ◑ 6 + passthrough | ✅ 100+ | ✅ 300+ |
| Battle-tested maturity | ◑ new | ✅ | ✅ |

> ✅ yes · ◑ partial · ❌ no. Adapters ship **beta/experimental** until verified
> against live provider APIs — see [PARITY.md](PARITY.md).

---

## Why gateway-first

LiteLLM is **library-first** (a Python SDK), which structurally traps it in
Python. llmux is **gateway-first**: the OpenAI HTTP schema is the canonical
interface, providers are adapters behind it, and the language ecosystems already
wrote the clients. We write the gateway once; you get every language free.

Three rules keep "any language" true as features grow:

1. The OpenAI HTTP schema is the canonical contract — provider quirks never leak.
2. Routing / budget controls ride on standard fields + `extra_headers` / `metadata` — no custom client is ever needed.
3. Streaming is **byte-identical** to OpenAI SSE — every language's stream parser just works.

---

## Pricing catalog — free, live, and route-correct

A seed ships built-in so cost works offline. At runtime llmux auto-syncs from
pluggable **sources** and merges them by **precedence** so cost is correct per route:

```text
override (manual pin) > provider pricing API > LiteLLM (direct) > OpenRouter (margin) > built-in seed
```

- **Route-aware:** a call routed *through* OpenRouter is costed at its
  margin-inclusive price; a **direct** BYO-key call prefers the authoritative
  direct price — so you're never over-charged on direct routes.
- **Manual overrides** (inline or hot-reloaded JSON) always win.
- **Disk cache** for instant warm starts and offline survival.
- **Open export:** `GET /v1/catalog.json` republishes the merged catalog.

---

## API

| Endpoint | Purpose |
|---|---|
| `POST /v1/chat/completions` | chat — streaming + non-streaming |
| `POST /v1/embeddings` | embeddings |
| `POST /v1/completions` · `/moderations` · `/images/generations` · `/audio/speech` · `/rerank` · `/responses` | modality routes (forwarded) |
| `GET /v1/models` | catalog-backed model list with pricing + capabilities |
| `GET /v1/catalog.json` | merged price catalog export |
| `GET /health` · `GET /metrics` | health + Prometheus metrics |

---

## Architecture

```text
core/                MIT — the open gateway
  openai/            canonical wire types (the contract)
  server/            HTTP gateway, streaming, auth, metrics, usage
  provider/          Provider interface + SSE utils
    passthrough/     OpenAI-shaped upstreams
    anthropic/ gemini/ cohere/ bedrock/ azure/   native adapters
  router/            routing + least-cost
  keys/              virtual keys, budgets, rate limits
  cache/             exact + semantic response cache
  pricing/           catalog + live sync + cost
cmd/llmux/           the binary (server + local sidecar)
web/                 Vite + React UI (embedded at /ui)
sdks/                thin language packages (python, node, go)
ee/                  enterprise/cloud (open-core)
```

The **same binary** is both the hosted server and the locally-embedded sidecar —
one codebase, two distribution modes.

---

## Development

```bash
make build      # build the binary
make web        # rebuild the embedded web UI
make test       # all Go tests (-race)
make docker     # build the Docker image
```

---

## License

**MIT** — see [LICENSE](LICENSE). The whole project is open source under MIT;
monetization is the hosted **[llmux Cloud](https://llmux.to)**, not a different
code license. See [ee/README.md](ee/README.md) for the Cloud/enterprise direction.

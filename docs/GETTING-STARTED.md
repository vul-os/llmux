# Getting Started with llmux

llmux is the sovereign LLM gateway: a single Go binary that speaks the OpenAI HTTP API and routes every request to the provider behind it — a local model on your box, or (only when you explicitly opt in) a remote provider such as OpenAI, Anthropic, Gemini, Cohere, Bedrock or Azure. Point any OpenAI SDK at it and routing, budgets, caching and live cost accounting happen underneath; in a Vulos deployment it is also the one endpoint every product's AI features call. This guide takes you from zero to a running gateway: building/deploying it, connecting providers (including keeping inference on-box), pointing Vulos OS and its apps at it with `LLMUX_URL`, and setting up authentication.

## 1. Deploy llmux

**Prerequisites:** Go 1.25+ and Node 18+ (Node only if you rebuild the embedded web UI). No database or cache is required for a single replica.

### Build and run from source

```bash
git clone https://github.com/llmux/llmux
cd llmux
make build                      # builds ./dist/llmux with the web UI embedded

cp llmux.example.json llmux.json
./dist/llmux -config llmux.json # gateway on :4000, dashboard at /ui
```

The config path comes from `-config` or the `LLMUX_CONFIG` env var. Useful `make` targets: `make web` (rebuild the admin SPA), `make run`, `make docker`, `make test`.

### Docker

The repo ships a `Dockerfile`:

```bash
docker build -t llmux .
docker run -p 4000:4000 \
  -e LLMUX_MASTER_KEY=$(openssl rand -hex 32) \
  -e OPENAI_API_KEY=sk-... \
  -v $PWD/llmux.json:/etc/llmux/llmux.json \
  llmux -config /etc/llmux/llmux.json
```

### First checks

```bash
curl http://localhost:4000/health          # {"status":"ok"}
open http://localhost:4000/ui              # embedded dashboard (usage, keys, catalog, docs)
./dist/llmux models                        # CLI: models with pricing + context window
```

`GET /health` needs no auth. The dashboard at `/ui` is public static content; its admin views talk to the master-key-gated `/admin/*` API.

### Scaling up (optional)

Everything works in-memory on one replica. For multiple replicas, add:

- **Postgres** — persists virtual keys and per-key spend. Set `VULOS_DATABASE_URL` or `DATABASE_URL` (shared DSN; llmux's tables live under their own schema, default `llmux`, override with `LLMUX_POSTGRES_SCHEMA`), or the legacy `LLMUX_POSTGRES`. Key tokens are stored as `sha256(token)`, never plaintext.
- **Redis** — backs per-key rate limits and the shared response cache. Set `LLMUX_REDIS` (`host:port`).

### Response caching (optional)

Identical requests can be answered from a per-key cache instead of hitting a provider:

```jsonc
{
  "cache": {
    "enabled": true,
    "ttl_seconds": 300,          // 0 = no expiry
    "max_entries": 10000,        // LRU bound
    "semantic": false            // true = embedding-similarity matching too
  }
}
```

Cache hits carry `cached: true` in the usage record and cost nothing upstream. Caches are scoped per virtual key, so keys never see each other's completions. See [ADMIN-GUIDE.md](./ADMIN-GUIDE.md#response-caching) for the semantic-cache settings.

## 2. Connect providers

Providers are declared in the JSON config (`providers[]`), each with a `name`, a `type`, a `base_url` and a credential. The accepted `type` values — the complete list, validated at startup — are:

| `type` | What it talks to |
|---|---|
| `passthrough` | Any OpenAI-shaped upstream: OpenAI itself, DeepSeek, Groq, Mistral, Together, Fireworks, xAI, OpenRouter, a local Ollama/llama.cpp/vLLM server, … |
| `anthropic` | Anthropic Messages API (native translation) |
| `gemini` | Google Gemini (native translation) |
| `cohere` | Cohere v2 (native translation) |
| `bedrock` | AWS Bedrock (Anthropic Claude models, SigV4 signing) |
| `azure` | Azure OpenAI |

Credentials come from `api_key` (inline) or, preferably, `api_key_env` (name of an env var). Bedrock reads standard `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`.

### Zero-config auto-detection

With no providers in the config, llmux auto-detects from the environment. Setting any of these env vars materialises the matching provider automatically:

`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `GROQ_API_KEY`, `MISTRAL_API_KEY`, `TOGETHER_API_KEY`, `FIREWORKS_API_KEY`, `XAI_API_KEY`, `OPENROUTER_API_KEY`, `COHERE_API_KEY`.

A **local, on-box provider** (named `local`) is likewise auto-detected from `OLLAMA_HOST` (an Ollama server; llmux appends `/v1`) or `LLMUX_LOCAL_BASE_URL` (any OpenAI-compatible local server), with optional `LLMUX_LOCAL_API_KEY`.

### The sovereignty gate — where your AI runs

This is the part that makes llmux *sovereign*: a default-deny egress gate runs before **every** dispatch. A loopback/unix-socket `base_url` is `local` and always allowed; **any off-box endpoint is blocked until you opt it in, per provider**:

```jsonc
{
  "providers": [
    { "name": "local",  "type": "passthrough", "base_url": "http://127.0.0.1:11434/v1" },
    { "name": "openai", "type": "passthrough",
      "base_url": "https://api.openai.com/v1",
      "api_key_env": "OPENAI_API_KEY",
      "allow_egress": true },                          // explicit opt-in for off-box
    { "name": "broker", "type": "passthrough",
      "base_url": "https://inference.example.com/v1",
      "tier": "brokered", "allow_brokered": true }     // named no-train third party
  ]
}
```

Provider fields for the gate: `"tier": "sovereign"` (an off-box endpoint you personally vouch for), `"tier": "brokered"` + `"allow_brokered": true` (a named third party under a claimed no-train agreement), or `"allow_egress": true` (the broad escape hatch for plain external providers). A blocked provider is never dialed; the request gets a 403 (`sovereignty_error` / `egress_not_allowed`), the denial is counted in the `llmux_egress_blocked_total` metric, and every *permitted* off-box call is logged with its tier. If you forget to opt a remote provider in, that 403 is the symptom — see [TROUBLESHOOTING.md](./TROUBLESHOOTING.md).

### Routes: how a model name picks a provider

The request's `model` string is matched against `routes[]`:

```jsonc
{
  "routes": [
    { "model": "assistant", "provider": "local",  "target_model": "llama3" },  // alias
    { "model": "claude-*",  "provider": "anthropic" },                         // wildcard prefix
    { "model": "cheapest", "strategy": "least-cost",                          // least-cost pick
      "candidates": [ { "provider": "openai", "model": "gpt-4o-mini" },
                      { "provider": "local",  "model": "llama3" } ] },
    { "model": "*", "provider": "local", "fallbacks": ["openai"] }             // catch-all + fallback
  ]
}
```

Resolution order: exact match → longest trailing-`*` wildcard → the `*` catch-all → and if no route matches at all, `provider/model` prefix syntax (e.g. `openai/gpt-4o`) routes directly to a named provider. `fallbacks` name providers to try in order when the primary fails (retryable statuses: 429/500/502/503/504 and transport errors), with global `retry` settings (`max_retries` default 2, `backoff_ms` default 200, exponential). A sovereignty-blocked primary is skipped so a local fallback can still serve.

## 3. Make your first request

Any OpenAI client works unchanged — the base URL is `http://<host>:4000/v1`:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:4000/v1", api_key="sk-team-a")
resp = client.chat.completions.create(
    model="assistant",
    messages=[{"role": "user", "content": "hi"}],
)
print(resp.usage)     # includes per-request cost_usd from the live catalog
```

Or raw curl:

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-team-a" \
  -H "Content-Type: application/json" \
  -d '{"model":"assistant","messages":[{"role":"user","content":"Hello!"}]}'
```

Streaming (`"stream": true`) returns byte-identical OpenAI SSE. Besides `chat/completions`, the gateway serves `/v1/embeddings`, `/v1/models`, `/v1/catalog.json`, and forwarded modality routes: `/v1/completions`, `/v1/responses`, `/v1/rerank`, `/v1/moderations`, `/v1/images/generations`, `/v1/audio/speech`. Note the forwarded modality routes are served by `passthrough`-type providers; the translating adapters (anthropic, bedrock, …) answer them with 501. Copy-paste examples for 17+ languages live at `/ui/docs` in the running gateway.

## 4. Auth and keys

Three layers, all optional but strongly recommended in that order:

### Master key

```bash
export LLMUX_MASTER_KEY=$(openssl rand -hex 32)
```

The master key (also settable as `server.master_key` in config) gates the admin surface: `/admin/keys`, `/admin/usage`, `/admin/byok/*`, `/metrics`, and the full provider/sovereignty disclosure on `/health`. It is also accepted on `/v1/*`. Virtual keys are **never** accepted on `/admin/*`.

**Keyless mode** (no master key, no virtual keys) is for local development only: the gateway then refuses to bind a non-loopback address unless you explicitly set `LLMUX_INSECURE_KEYLESS=1`, and on loopback `/admin` and `/metrics` are open to local callers.

### Virtual keys

Virtual keys are what applications hold. Each carries its own controls:

```jsonc
{
  "keys": [
    { "key": "sk-team-a", "name": "team-a",
      "budget_usd": 25.0,                       // 0 = unlimited
      "rpm": 60,                                 // 0 = unlimited
      "allowed_models": ["assistant", "cheapest"] }  // empty = all models
  ]
}
```

Requests authenticate with `Authorization: Bearer <virtual key>`. Over budget returns HTTP 402 (`budget_exceeded`), over the RPM limit returns 429, and a model outside the allow-list returns 403 (`model_not_allowed`). Spend persists to `key_store_path` (JSON) or Postgres; tokens are hashed at rest.

### Per-account BYOK (optional)

With a key-encryption key configured (`LLMUX_BYOK_KEK`, 32 bytes; optional persistent store `LLMUX_BYOK_STORE`), individual accounts can register their *own* provider keys via `PUT /admin/byok/{account}/{provider}` — those requests then use the account's key and are never metered/billed centrally. Without a KEK the BYOK endpoints return 501. Full semantics in [LLM-ACCESS.md](./LLM-ACCESS.md).

## 5. Point Vulos OS and apps at llmux

In a Vulos deployment, products never talk to providers — they talk to llmux, and mostly indirectly through Vulos OS. Two env vars on the **OS backend** wire it up:

| Env var (Vulos OS) | Purpose |
|---|---|
| `LLMUX_URL` (alias: `VULOS_LLMUX_URL`) | Base URL of the llmux gateway, e.g. `http://127.0.0.1:4000` or `http://127.0.0.1:4000/v1` (a trailing `/v1` is stripped and re-appended per call). **No default** — with it unset, the OS's `/api/ai/*` routes return 503. |
| `LLMUX_KEY` (alias: `VULOS_LLMUX_KEY`) | The bearer sent on every forwarded call (`Authorization: Bearer …`) — typically a llmux **virtual key** minted for the OS. Optional for an unauthenticated loopback gateway. |

What that enables:

- **The OS AI gateway** — `POST /api/ai/chat` forwards the raw OpenAI-shaped body to llmux `/v1/chat/completions` (SSE streamed back verbatim; the caller must supply `model`), `POST /api/ai/models` lists models, `POST /api/ai/embed` and the notes-index/search routes use `/v1/embeddings` (default embedding model `text-embedding-3-small`), and `GET /api/ai/status` reports `unconfigured` vs `ok` + gateway URL. Shell chat, the browser SmartBar and Notes semantic search all ride these routes.
- **The mail assistant ("Vula")** — the `/api/assistant/*` routes switch their model backend to llmux automatically when `LLMUX_URL` is set (provider `custom`, endpoint = llmux, key = `LLMUX_KEY`; the model name still comes from `AI_MODEL`, default `llama3`). Without `LLMUX_URL` the assistant falls back to direct Ollama (`AI_ENDPOINT`, default `http://localhost:11434`).
- **Sovereignty end-to-end** — the OS assistant applies the same four-tier dial (local/sovereign/brokered/external) to its own endpoint: run llmux on loopback (recommended: `http://127.0.0.1:4000`) and mail content stays classified "On your device". Pointing the OS at a non-loopback llmux is blocked unless `VULOS_ASSISTANT_ALLOW_EXTERNAL=1`.

Notes: the old in-OS "airouter" is gone — `PUT /api/ai/config` returns 410 (`airouter_removed: configure providers in llmux`); provider configuration now lives entirely in llmux. Speech-to-text does **not** route through llmux (it lacks the multipart `/v1/audio/transcriptions` shape) — the OS uses `VULOS_WHISPER_URL`/`VULOS_WHISPER_KEY` separately.

Any non-Vulos app is even simpler: give it `base_url = http://<llmux-host>:4000/v1` and a virtual key.

## 6. Environment variable reference

Gateway (all read at startup; config-file values noted where different):

| Env var | Purpose |
|---|---|
| `LLMUX_CONFIG` | Path to the JSON config (alternative to `-config`) |
| `LLMUX_ADDR` | Listen address (default `:4000`) |
| `LLMUX_SOCKET` | Listen on a unix socket instead |
| `LLMUX_MASTER_KEY` | Admin/master key |
| `LLMUX_INSECURE_KEYLESS` | `1`/`true`: allow keyless bind on a non-loopback address (dangerous) |
| `LLMUX_LOG_LEVEL` | Log verbosity |
| `VULOS_DATABASE_URL` / `DATABASE_URL` / `LLMUX_POSTGRES` | Postgres DSN (that order of precedence, highest first) |
| `LLMUX_POSTGRES_SCHEMA` | Schema for llmux's tables (default `llmux`) |
| `LLMUX_REDIS` | Redis `host:port` |
| `LLMUX_SYNC_INTERVAL_MIN` | Pricing-catalog sync interval (minutes) |
| `LLMUX_USAGE_LOG` | Path for the local JSONL usage ledger (env-only; no config key) |
| `LLMUX_BYOK_KEK` / `LLMUX_BYOK_STORE` | Enable + persist per-account BYOK keys |
| `LLMUX_CP_URL` / `LLMUX_CP_SECRET` / `LLMUX_CP_RPM` / `LLMUX_CP_DEGRADED_RPM` / `LLMUX_CP_DEGRADED_FAIL_OPEN` | Optional control-plane billing seam (see [ADMIN-GUIDE.md](./ADMIN-GUIDE.md)) |
| `OLLAMA_HOST` / `LLMUX_LOCAL_BASE_URL` / `LLMUX_LOCAL_API_KEY` | Auto-detect the on-box `local` provider |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `GROQ_API_KEY`, `MISTRAL_API_KEY`, `TOGETHER_API_KEY`, `FIREWORKS_API_KEY`, `XAI_API_KEY`, `OPENROUTER_API_KEY`, `COHERE_API_KEY` | Provider credentials (also auto-detect those providers) |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` | Bedrock credentials |

## 7. Production checklist

Before exposing the gateway beyond your own machine:

- [ ] `LLMUX_MASTER_KEY` is set (keyless mode is loopback-only by design).
- [ ] Every application has its **own virtual key** with a `budget_usd` and `rpm` — nothing holds the master key except you.
- [ ] Off-box providers are opted in **deliberately** (`allow_egress` / `tier`), and `GET /health` with the master key shows exactly the posture you intended.
- [ ] A local provider (Ollama/llama.cpp/vLLM via `passthrough`) is configured as the default route — sovereign by default, and a fallback when remote providers are down.
- [ ] `LLMUX_USAGE_LOG` is set if you need an audit trail or bill through the control plane.
- [ ] Behind a reverse proxy: response buffering is disabled for `/v1/chat/completions` (SSE).
- [ ] Multiple replicas only: Postgres (`VULOS_DATABASE_URL`/`DATABASE_URL`) and Redis (`LLMUX_REDIS`) are wired so keys, spend, rate limits and cache stay consistent.
- [ ] You've read [HARDENING.md](../HARDENING.md).

## 8. Where to next

- [ADMIN-GUIDE.md](./ADMIN-GUIDE.md) — budgets, metering, model routing in depth, logging/privacy posture.
- [TROUBLESHOOTING.md](./TROUBLESHOOTING.md) — 503s from `/api/ai/*`, provider auth failures, budget exhaustion, streaming issues.
- [LLM-ACCESS.md](./LLM-ACCESS.md) — BYOK vs central metering, the product consumption contract.
- [configuration.md](./configuration.md) and [`llmux.example.json`](../llmux.example.json) — the full config surface.
- [HARDENING.md](../HARDENING.md) — production security posture.

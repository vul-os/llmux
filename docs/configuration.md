# Configuration

llmux is configured by a single JSON file. Common settings can also be overridden
by environment variable, which is convenient for containers and secrets managers.

The config path is passed with `-config` or read from `LLMUX_CONFIG`:

```bash
./dist/llmux -config llmux.json
# or
LLMUX_CONFIG=llmux.json ./dist/llmux
```

See [`llmux.example.json`](../llmux.example.json) for a full, commented example
covering providers, routes, fallbacks, least-cost candidates, caching, virtual
keys, and the pricing catalog.

## Environment variables

| Env var | Purpose |
|---|---|
| `LLMUX_CONFIG` | Path to the JSON config file |
| `LLMUX_ADDR` | Listen address (default `:4000`) |
| `LLMUX_MASTER_KEY` | Admin/master key for `/admin`, `/metrics` |
| `VULOS_DATABASE_URL` | Shared Postgres DSN (Vulos-specific; **preferred**). Virtual keys + spend live under schema `llmux`. See below. |
| `DATABASE_URL` | Shared Postgres DSN (standard). Same effect as `VULOS_DATABASE_URL`; lower precedence. |
| `LLMUX_POSTGRES` | Postgres DSN (virtual keys + spend). Legacy fallback — used only if no shared DSN is set. |
| `LLMUX_POSTGRES_SCHEMA` | Postgres schema for llmux's tables (default `llmux`). |
| `LLMUX_REDIS` | Redis address (rate limits + shared cache) |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, … | Provider credentials, referenced by `api_key_env` in config |
| `LLMUX_CP_URL`, `LLMUX_CP_SECRET` | Optional control-plane URL + shared secret (see [Control-plane seam](control-plane.md)) |
| `LLMUX_BYOK_KEK` | 32-byte key-encryption key (raw / 64-hex / base64) enabling per-account BYOK keys, encrypted at rest. Empty = BYOK off (see [LLM access](LLM-ACCESS.md)) |
| `LLMUX_BYOK_STORE` | Path to persist the encrypted BYOK store (omit for in-memory only) |
| `LLMUX_LOG_LEVEL` | Log verbosity |

## Postgres & Redis are optional

For single-replica use, virtual keys and the response cache work entirely
in-memory — no external dependencies required. Configure Postgres and Redis when
you run **multiple replicas**, so keys, spend, rate limits, and cache stay
consistent across them:

- **Postgres** — persists virtual keys and per-key spend.
- **Redis** — backs per-key rate limits and the shared response cache.

## Postgres DSN & shared-database (cloud consolidation)

llmux resolves its Postgres DSN from several sources so it can either run with its
own database or share one database (e.g. a single Neon database) with the other
Vulos products. Resolution order (**later wins**):

1. `postgres` in the config file
2. `LLMUX_POSTGRES` — legacy, product-specific fallback (kept working)
3. `DATABASE_URL` — the standard shared DSN
4. `VULOS_DATABASE_URL` — the Vulos-specific shared DSN (highest precedence)

A shared DSN (`DATABASE_URL` / `VULOS_DATABASE_URL`) is therefore **preferred**
over `LLMUX_POSTGRES`. With **no** DSN set at all, llmux uses its in-memory /
embedded default (single-replica; no external dependency) — unchanged.

**Dedicated schema.** Whenever Postgres is in use, all of llmux's tables live
under a dedicated schema (default **`llmux`**, override with
`LLMUX_POSTGRES_SCHEMA` or `postgres_schema` in the config file). The schema is
created automatically (`CREATE SCHEMA IF NOT EXISTS`) and the keys table is
created as `llmux.llmux_keys`. This lets llmux share one database with the other
products without name collisions.

```bash
# Share the Vulos Neon database; llmux's tables go under schema "llmux".
DATABASE_URL='postgres://user:pass@ep-xyz.eu-central-1.aws.neon.tech/vulos?sslmode=require' ./dist/llmux
```

**Keys hashed at rest.** Bearer tokens are never stored in plaintext: the
`llmux.llmux_keys.key` column holds `sha256(token)` (and Redis rate-limit keys
use the same hash), so a database dump never yields live credentials. This holds
on the shared-schema path too.

## Sovereignty (per-provider egress)

Each provider is classified by **where its traffic goes** and gated by a
default-deny egress policy before every dispatch (see
[the sovereignty gate](architecture.md#the-sovereignty-gate-where-your-ai-runs)).
A loopback / unix-socket `base_url` is **local** and always allowed; any off-box
endpoint is **external** and **blocked** by default. You opt in per-provider —
never globally — with these provider fields:

| Provider field | Effect |
|---|---|
| `"tier": "sovereign"` | Declares this off-box endpoint one you vouch for (unverified by Vulos). Allowed without `allow_egress`. |
| `"tier": "brokered"` | Declares a named third party under a claimed no-train agreement. Allowed only with `allow_brokered` (or `allow_egress`). |
| `"allow_brokered": true` | Permits calls to `brokered`-tier providers. |
| `"allow_egress": true` | Permits calls to a plain **external** off-box provider (the broad escape hatch). |

A loopback URL is always `local` regardless of marking (you can't mislabel an
on-box endpoint), and the gate fails **closed**: an empty/unparseable `base_url`,
an off-box endpoint marked `local`, or an unknown tier value is treated as
external and blocked. Blocked providers are never dialed; permitted off-box calls
are logged with their tier, and `GET /health` (master key) discloses the full
posture. See the sovereignty block in
[`llmux.example.json`](../llmux.example.json).

## Related

- [Routing & reliability](../web/docs/routing.md) — how requests map to providers
- [Pricing & cost](../web/docs/pricing.md) — the catalog and cost accounting
- [Hardening](../HARDENING.md) — production posture

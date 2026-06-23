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
| `LLMUX_POSTGRES` | Postgres DSN (virtual keys + spend) |
| `LLMUX_REDIS` | Redis address (rate limits + shared cache) |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, … | Provider credentials, referenced by `api_key_env` in config |
| `LLMUX_CP_URL`, `LLMUX_CP_SECRET` | Optional control-plane URL + shared secret (see [Control-plane seam](control-plane.md)) |
| `LLMUX_LOG_LEVEL` | Log verbosity |

## Postgres & Redis are optional

For single-replica use, virtual keys and the response cache work entirely
in-memory — no external dependencies required. Configure Postgres and Redis when
you run **multiple replicas**, so keys, spend, rate limits, and cache stay
consistent across them:

- **Postgres** — persists virtual keys and per-key spend.
- **Redis** — backs per-key rate limits and the shared response cache.

## Related

- [Routing & reliability](../web/docs/routing.md) — how requests map to providers
- [Pricing & cost](../web/docs/pricing.md) — the catalog and cost accounting
- [Hardening](../HARDENING.md) — production posture

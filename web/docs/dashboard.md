# Dashboard & ops

A web dashboard ships **inside the binary** at `/ui` — no separate service.

![llmux dashboard](/ui/shots/dashboard.jpg)

## What it shows

- **Usage** — totals and per-model breakdown (requests, tokens, cost).
- **Keys** — virtual keys with budget, live spend, and rate limit (masked).
- **Models** — the live price catalog with input/output rates and context.

It authenticates to the admin endpoints with the master key, which you enter in
the settings bar (kept client-side).

## Endpoints behind it

| Endpoint | Purpose |
|----------|---------|
| `GET /admin/usage` | aggregate usage by key/model (master key) |
| `GET /admin/keys`  | virtual keys + live spend (master key) |
| `GET /metrics`     | Prometheus metrics (master key) |
| `GET /health`      | status + provider stability tiers |

## Governance

```json
{
  "keys": [
    { "key": "sk-team-a", "name": "team-a", "budget_usd": 100, "rpm": 600,
      "allowed_models": ["gpt-4o", "cheapest"] }
  ],
  "key_store_path": "keys.json"
}
```

For multi-replica deployments, set `postgres` (keys/spend) and `redis`
(rate limits + cache) so budgets and limits are correct across instances.

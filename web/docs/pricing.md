# Pricing & cost

llmux tracks per-request cost from a **live, auto-synced** price catalog and
returns it in every response's `usage` block.

## Sources & precedence

The catalog merges multiple sources by trust:

```
override (manual)  >  provider API (Azure)  >  LiteLLM (direct)  >  OpenRouter (margin)  >  built-in seed
```

- **Route-aware** — a call *through* OpenRouter is costed at OpenRouter's
  margin-inclusive price; a **direct** BYO-key call uses the authoritative
  direct price, so you're never over-charged.
- **Cached tokens** — `prompt_tokens_details.cached_tokens` are billed at the
  provider's discounted cache-read rate.
- **Overrides** — pin or correct any model's price inline or via a JSON file.

## Config

```json
{
  "pricing": {
    "sync_interval_minutes": 360,
    "catalog_path": "catalog.json",
    "sources": [
      "https://openrouter.ai/api/v1/models",
      "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
    ],
    "overrides": {
      "openai/gpt-4o": { "input_per_mtok": 2.5, "output_per_mtok": 10.0 }
    }
  }
}
```

## Endpoints

- `GET /v1/models` — catalog-backed list with pricing + capabilities
- `GET /v1/catalog.json` — the merged catalog as open JSON (consume it freely)

Every response includes:

```json
{ "usage": { "prompt_tokens": 12, "completion_tokens": 8,
  "cost": { "input_cost": 0.00003, "output_cost": 0.00008, "total_cost": 0.00011, "currency": "USD" } } }
```

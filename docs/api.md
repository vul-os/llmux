# API reference

llmux speaks the **OpenAI HTTP API**. Point any OpenAI client at
`http://<host>:4000/v1` and the model string selects the route. This page lists
the served endpoints; for request/response field semantics, the
[OpenAI API reference](https://platform.openai.com/docs/api-reference) applies
verbatim.

## Authentication

| Surface | Credential |
|---|---|
| `/v1/*` | `Authorization: Bearer <virtual key>` |
| `/admin/*`, `/metrics` | The master key (`LLMUX_MASTER_KEY`) |

Virtual keys are defined in config (or resolved via the
[control-plane](control-plane.md)) and carry budgets, RPM limits, and model
allow-lists. See [Configuration](configuration.md).

## Inference endpoints

| Method & path | Notes |
|---|---|
| `POST /v1/chat/completions` | The primary endpoint. Routed + translated per provider; supports streaming (`"stream": true`) with byte-identical OpenAI SSE. |
| `POST /v1/embeddings` | Embeddings, routed per provider. |
| `POST /v1/completions` | Legacy text completions (forwarded). |
| `POST /v1/responses` | OpenAI Responses API (forwarded; usage mapped to canonical fields). |
| `POST /v1/rerank` | Reranking (forwarded). |
| `POST /v1/moderations` | Moderation (forwarded). |
| `POST /v1/images/generations` | Image generation (forwarded). |
| `POST /v1/audio/speech` | Text-to-speech (forwarded). |

> **Routed vs. forwarded.** `chat/completions` and `embeddings` go through native
> per-provider translation. The other modality routes are forwarded to the
> resolved upstream; every *served* forward is still metered. See
> [Providers](../web/docs/providers.md).

## Catalog & discovery

| Method & path | Notes |
|---|---|
| `GET /v1/models` | Available models with pricing and context window. |
| `GET /v1/catalog.json` | The merged, live price catalog. |

## Admin & ops

| Method & path | Auth | Notes |
|---|---|---|
| `GET /admin/keys` | master key | Virtual keys: budgets, spend, RPM. |
| `GET /admin/usage` | master key | Usage by model. |
| `GET /admin/byok/{account}` | master key | List provider names an account has BYOK keys for (never the keys). See [LLM access](LLM-ACCESS.md). |
| `PUT /admin/byok/{account}/{provider}` | master key | Set (encrypt) an account's own provider key → that provider goes BYOK (unmetered). |
| `DELETE /admin/byok/{account}/{provider}` | master key | Clear an account's BYOK key → revert to central (metered). |
| `GET /metrics` | master key | Prometheus metrics. |
| `GET /health` | none (minimal `{"status":"ok"}` for any caller); master key (or loopback when keyless) additionally unlocks the full provider/sovereignty topology | Liveness probe. |
| `GET /ui`, `GET /ui/docs` | none | Embedded admin dashboard + docs. |

## Example

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-team-a" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

The model string (`claude-3-5-sonnet` above) is matched against your
[routes](../web/docs/routing.md): an alias, a `provider/model` prefix, a wildcard,
a least-cost pseudo-model like `cheapest`, or the catch-all.

## Cost in every response

Each response's `usage` block includes the computed per-request cost, drawn from
the live [pricing catalog](../web/docs/pricing.md). Streaming responses are
metered too — llmux forces a final usage chunk and falls back to a token estimate
if the upstream omits one.

## Errors

Upstream and validation errors are normalized to the OpenAI error shape
(`{"error": {"message", "type", "code"}}`) with a faithful HTTP status, so client
SDK error handling works unchanged. Rate-limit and `retry-after` headers from the
upstream are relayed.

Alongside relayed upstream errors, the gateway raises these of its own before it
ever calls a provider:

| Status | `code` | When |
|---|---|---|
| 401 | `invalid_api_key` | The bearer token is unknown (or the control plane rejected it). |
| 400 | `missing_model` | No `model` in the request body. |
| 403 | `model_not_allowed` | The key's model allow-list doesn't include the requested model. |
| 404 | `model_not_found` | No route matches the model string. |
| 403 | `model_not_priced` | A **budgeted** key requested a routable model the catalog cannot price. Refused pre-flight so an unmeterable request can't burn unbounded real provider spend while logging $0. Price the model (or the pricing sync will) or scope the key. BYOK requests and keys with no budget are unaffected — the latter serve the model and log it at $0. |
| 402 | `budget_exceeded` | The key/account is over its USD budget. **Fail-closed:** if the spend store (Postgres) cannot be read and no last-known-good figure exists, the key is treated as over budget rather than unspent. |
| 429 | `rate_limit_exceeded` | The key's RPM limit (or a control-plane per-account cap) was hit. |
| 403 | `egress_not_allowed` | The [sovereignty gate](architecture.md#the-sovereignty-gate-where-your-ai-runs) blocked an off-box provider the operator has not opted in. |

## Related

- [Routing & reliability](../web/docs/routing.md)
- [Providers](../web/docs/providers.md)
- [Pricing & cost](../web/docs/pricing.md)
- [Configuration](configuration.md)

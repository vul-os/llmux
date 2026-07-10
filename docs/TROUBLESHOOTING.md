# llmux Troubleshooting

This page maps the symptoms you actually see — a 503 from a Vulos app's `/api/ai/*` route, a provider rejecting your key, a request denied for budget, a stream that stalls or never ends — to their causes and fixes. Status codes and error `code` strings quoted here are the exact ones the gateway (and the Vulos OS proxy in front of it) emit, so you can match them against what's in your client or logs. When in doubt, start with `curl http://<gateway>:4000/health` and the gateway's structured logs.

## Symptom index

| You see | Most likely | Section |
|---|---|---|
| 503 `gateway_unconfigured` from a Vulos app | `LLMUX_URL` unset on the OS | 503s from `/api/ai/*` |
| 502 `gateway_error` / `embed_error` from a Vulos app | llmux configured but failing | 503s from `/api/ai/*` |
| 403 `egress_not_allowed` | Sovereignty gate blocking an off-box provider | The sovereignty gate |
| 401/403 relayed from a provider | Wrong/missing provider key, or a BYOK key | Provider auth failures |
| 402 `budget_exceeded` | Key budget or account managed credit spent | Budget and rate-limit denials |
| 429 `rate_limit_exceeded` | Key RPM, account RPM, or CP degraded cap | Budget and rate-limit denials |
| 403 `model_not_allowed` | Key's `allowed_models` | Budget and rate-limit denials |
| 404 `model_not_found` | No route matches the model string | Budget and rate-limit denials |
| Stream stalls / cut off | Proxy buffering, or mid-stream upstream failure | Streaming issues |
| 501 on `/v1/responses`, images, audio, … | Modality route hit a translating adapter | Modality routes returning 501 |
| `cost_usd` 0 / usage missing from CP | Catalog gap / BYOK / CP retry queue drained | Cost / metering anomalies |
| Gateway won't start | Bad provider `type`, keyless non-loopback bind | Startup problems |

## 503s from `/api/ai/*` (Vulos OS routes)

These come from **Vulos OS**, not llmux itself — the OS proxies `/api/ai/*` to llmux and returns 503 when it can't.

| Symptom | Cause | Fix |
|---|---|---|
| `{"error":"gateway_unconfigured: set LLMUX_URL"}` on `POST /api/ai/chat` | `LLMUX_URL` (or `VULOS_LLMUX_URL`) is not set on the OS backend. There is no default. | Set `LLMUX_URL=http://127.0.0.1:4000` (loopback recommended for sovereignty) and restart the OS backend. |
| 503 `{"error":"gateway_error: …"}` on `POST /api/ai/models` | Any failure talking to llmux — unset URL, gateway down, or a non-2xx reply. | Check `GET /api/ai/status` (`unconfigured` vs `ok` + gateway URL), then `curl <LLMUX_URL>/health`. |
| 503 `{"error":"request_cancelled"}` on chat | The client disconnected while queued — the OS caps concurrent chat forwards at 10 per instance. | Retry; if chronic, reduce parallel AI calls or run more OS instances. |
| 503 `{"error":"note_embeddings_unavailable: store not initialised"}` | The OS-local embedding store isn't up (notes indexing), unrelated to llmux. | Check the OS backend logs/storage. |

Related non-503s from the same surface: **502** `gateway_error`/`embed_error` means llmux is *configured but failing* (fix llmux, see below); **422** `model_not_found` means the request body had an empty `model` (the OS proxy forwards bodies verbatim — the caller must name a model); **429** `rate_limit_exceeded` is the OS's own per-account limiter (honour `Retry-After`); **410** on `PUT /api/ai/config` is permanent — the old airouter is gone, configure providers in llmux instead. Also remember the OS-side sovereignty guard: the mail assistant refuses a **non-loopback** `LLMUX_URL` unless `VULOS_ASSISTANT_ALLOW_EXTERNAL=1`.

## 403 `egress_not_allowed` — the sovereignty gate

```
sovereignty: provider "openai" is a non-local endpoint and egress is not
enabled; set "allow_egress": true on this provider to permit off-box calls
```

This is llmux working as designed: every off-box provider is **blocked by default**. If you intend that traffic, opt in per provider in the config — `"allow_egress": true` (plain external), or declare `"tier": "sovereign"` / `"tier": "brokered"` + `"allow_brokered": true`. Checks:

- `GET /health` with the master key shows every provider's tier, locality and `egress_allowed` — confirm what the gateway actually resolved.
- The gate fails **closed**: an empty/unparseable `base_url`, an off-box endpoint marked `local`, or an unknown tier is treated as external and blocked. Fix the `base_url` rather than fighting the tier.
- Watch the `llmux_egress_blocked_total` metric — a rising count is something repeatedly trying to leave the box.
- If a *route* mixes local and remote targets, a blocked primary is skipped and a local fallback can still serve; a 403 surfacing to the client means **every** target was blocked.

## Provider auth failures

Symptom: 401/403 errors originating from the upstream provider (llmux normalizes them to the OpenAI error shape and relays the status).

1. **Wrong or missing key.** Provider keys come from `api_key` or `api_key_env` in the provider block. Confirm the env var is set *in the gateway's environment* (llmux resolves it at dispatch, e.g. `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …). For Bedrock it's `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`(/`AWS_SESSION_TOKEN`).
2. **Auto-detected provider without its env var.** If you rely on zero-config detection, the provider only exists while its key env var is set.
3. **BYOK surprises.** If an account registered its own key (`PUT /admin/byok/{account}/{provider}`), *that* key is used for their calls to that provider — a revoked personal key breaks only that account. `GET /admin/byok/{account}` lists which providers are BYOK for them; `DELETE` reverts to central. Note BYOK is per (account, provider): an account's OpenAI key is never sent to Anthropic.
4. **Bedrock BYOK is rejected by design** (`400 byok_unsupported_provider`): Bedrock uses SigV4, not a bearer key — it is central-only.
5. **BYOK endpoints returning 501**: no KEK configured — set `LLMUX_BYOK_KEK` (32 bytes) to enable BYOK at all.

Distinguish from **llmux's own auth**: a 401 from llmux means the `Authorization: Bearer` wasn't a valid virtual key (or master key); on `/admin/*` only the master key is accepted, and on a keyless box admin routes work from loopback only.

## Budget and rate-limit denials

| Response | Meaning | Fix |
|---|---|---|
| **402** `insufficient_quota` / `budget_exceeded`, reason `budget exceeded for key <name>` | The virtual key's `budget_usd` is spent. | Raise the budget or rotate spend; check `GET /admin/keys` / the dashboard. |
| **402** with reason `llm budget exhausted` | Control-plane managed credit for the *account* is used up (e.g. the free-tier allowance). | Top up / upgrade via the CP, or have the account go BYOK (BYOK calls don't consume central credit). |
| **402** with reason `account suspended` or `llm not enabled for account` | CP entitlement denies LLM access outright. | Resolve on the control plane; entitlements are cached ~30 s, so allow a moment after fixing. |
| **429** `rate_limit_exceeded`, reason `rate limit exceeded for key <name>` | Per-key `rpm` bucket empty. | Wait or raise `rpm`. |
| **429** with reason `control plane unavailable; degraded rate limit for account <id>` | The CP is unreachable and llmux fell back to the conservative degraded cap (`cp_degraded_rpm`, default 20/min). | Restore CP connectivity; optionally raise `cp_degraded_rpm` or (spend risk) set `cp_degraded_fail_open`. |
| **403** `model_not_allowed` | The key's `allowed_models` doesn't include the requested model. | Use an allowed model or extend the list. |
| **404** `model_not_found` | No route matched and no `provider/model` prefix applied. | Add a route (or a `*` catch-all), or use `provider/model` syntax. |

Note the split: budget problems are **402**; only sovereignty and model-allow-list denials are **403**. A tip for over-budget confusion: each in-flight request holds a small ($0.05) reservation, so a key within cents of its budget can be denied while requests are still in flight.

## Streaming issues

- **Stream cut off / no final usage chunk expected?** llmux always forces a final usage chunk: it sets `stream_options.include_usage` upstream itself, and if the upstream still omits usage it estimates (~4 chars/token) so metering never records zero. Your SSE parser must tolerate the final usage-only chunk (any OpenAI-compatible parser does).
- **Fallback didn't kick in mid-stream.** Failover happens only *before the first chunk*; once bytes have flowed, a mid-stream upstream failure ends the stream (what was served is still metered). Retry the request.
- **Stream stalls behind a proxy.** llmux's streaming HTTP client deliberately has *no* timeout (long generations are legal) — stalls usually come from an intermediate proxy buffering SSE. The Vulos OS proxy already sets `X-Accel-Buffering: no` and flushes per read; do the same in your own reverse proxy (disable response buffering for `/v1/chat/completions` and `/api/ai/chat`).
- **Non-streaming requests timing out at exactly 10 minutes**: that's the default 600 s upstream client timeout for unary calls; long jobs should stream. `upstream_timeout_seconds` can tighten (not extend) unary deadlines.
- **413/400 on big requests**: request bodies are capped at 32 MiB.

## Modality routes returning 501

`/v1/completions`, `/v1/responses`, `/v1/rerank`, `/v1/moderations`, `/v1/images/generations`, `/v1/audio/speech` are *forwarded*, and only `passthrough`-type providers can serve them. Routing one of these to a translating adapter (anthropic, gemini, cohere, bedrock, azure) yields 501. Route modality models to a passthrough provider. Similarly, Gemini tool-calling has a known limitation: JSON-schema `$ref`/`$defs` in tool definitions are not resolved — inline your schemas for Gemini routes. And note there is no `/v1/audio/transcriptions` (which is why Vulos OS wires Whisper separately via `VULOS_WHISPER_URL`).

## Embedding failures

Embeddings ride `/v1/embeddings` and follow the same routing/sovereignty/budget rules as chat, plus a few of their own wrinkles:

- **Vulos OS `POST /api/ai/embed` returning 502 `embed_error`** — llmux is configured but the embeddings call failed. Test llmux directly: `curl <LLMUX_URL>/v1/embeddings -d '{"model":"text-embedding-3-small","input":"ping"}' -H "Authorization: Bearer $KEY"`. The OS defaults the embedding model to `text-embedding-3-small` — make sure a route resolves that name.
- **Notes semantic search "works" but finds nothing** — the OS degrades gracefully: with the gateway unconfigured or failing it returns `{"hits":[],"degraded":true}` with HTTP 200. Check `GET /api/ai/status`.
- **Semantic cache suddenly 403s** — the semantic cache's embedder is a dispatch like any other and passes the sovereignty gate; if `cache.embedding_model` routes to an off-box provider that isn't opted in, cache lookups are blocked. Route it to a local embedder or opt the provider in.

## Admin access confusion

- **401 on `/admin/*` with a virtual key** — expected: only the master key is accepted on the admin surface, ever.
- **401 on `/metrics`** — `/metrics` requires the master key when one is set; on a keyless box it is loopback-only.
- **`/health` doesn't show providers/tiers** — anonymous callers get only `{"status":"ok"}`; the topology block requires the master key (or loopback on a keyless box).
- **Dashboard at `/ui` loads but admin views are empty** — the SPA is public; its keys/usage views need the master key supplied in the dashboard itself.
- **BYOK endpoints return 501** — BYOK is disabled because no `LLMUX_BYOK_KEK` is configured.

## Cost / metering anomalies

- **`cost_usd` is 0 or missing for a model**: the model isn't in the pricing catalog. Check `GET /v1/catalog.json`, wait for a sync (`sync_interval_minutes`, default 360), or add a `pricing.overrides` entry.
- **Usage missing from the control plane**: BYOK requests are *never* sent to the CP (by design); central records are queued with bounded retry (1024 deep, 5 attempts) — a long CP outage or a crash can drop queued records from CP delivery. They remain in the local JSONL ledger **if** `LLMUX_USAGE_LOG` is set — set it.
- **Two identical requests, one free**: response caching (`cache.enabled`) — cached hits are marked `cached: true` and cost attribution follows the route's primary provider. Caches are scoped per virtual key, so a "cache miss" across two different keys is expected.

## Startup problems

- **`provider %q: unknown type %q`** — `type` must be one of `passthrough`, `anthropic`, `gemini`, `cohere`, `bedrock`, `azure`.
- **Refuses to bind a non-loopback address keyless** — set `LLMUX_MASTER_KEY` (right fix) or `LLMUX_INSECURE_KEYLESS=1` (wrong fix, dev only).
- **BYOK on with a TCP listener and no master key** — startup warns loudly: anyone who can reach the port could set/clear account keys. Set a master key.
- **Postgres surprises** — DSN precedence is `VULOS_DATABASE_URL` > `DATABASE_URL` > `LLMUX_POSTGRES` > config `postgres`; tables live under the `llmux` schema (`LLMUX_POSTGRES_SCHEMA` to change). A stale `LLMUX_POSTGRES` can be silently outranked by a shared DSN.

## Retry and failover behaviour (what to expect)

Knowing what the gateway does on failure saves you from misreading symptoms:

- Retryable failures are exactly HTTP **429, 500, 502, 503, 504** and transport-level errors. Each route target is retried up to `retry.max_retries` (default 2) with exponential backoff (`backoff_ms` default 200 → 200/400/800 ms), then the next `fallbacks` provider is tried.
- Any other 4xx from the upstream is returned to you immediately — no retry, no failover. A 401 from OpenAI will never "fail over" to Anthropic; fix the key.
- A sovereignty-blocked target is skipped without a dial — so a chain of `[remote, local]` still serves from `local` even when the remote isn't opted in.
- Upstream `retry-after` and rate-limit headers are relayed to you on the final response — honour them.

## Known limitations (as of this writing)

- **No `/v1/audio/transcriptions`** — speech-to-text is out of scope; Vulos OS wires Whisper separately (`VULOS_WHISPER_URL`).
- **Gemini tool schemas:** JSON-schema `$ref`/`$defs` are not resolved by the Gemini adapter — inline schemas for Gemini-routed tool use.
- **Adapter maturity:** `cohere` and `bedrock` are marked `experimental`, `anthropic`/`gemini`/`azure` `beta` (visible in `/health` per-provider `stability`). Keep a `stable` passthrough or local target in fallback chains.
- **Bedrock is central-only for BYOK** (SigV4, not a bearer key) — `PUT /admin/byok/{account}/bedrock` is rejected with `400 byok_unsupported_provider`.
- **No CP usage reconciler** — records that outlive the bounded retry queue reach only the local JSONL ledger (`LLMUX_USAGE_LOG`).

## Quick diagnostic sequence

```bash
curl http://localhost:4000/health                                  # gateway alive?
curl -H "Authorization: Bearer $LLMUX_MASTER_KEY" \
     http://localhost:4000/health                                  # provider tiers + egress posture
curl -H "Authorization: Bearer $LLMUX_MASTER_KEY" \
     http://localhost:4000/admin/keys                              # budgets / spend / rpm
curl http://localhost:4000/v1/models -H "Authorization: Bearer $KEY"  # what this key can see
curl http://localhost:4000/v1/chat/completions -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<route>","messages":[{"role":"user","content":"ping"}]}'
```

Then, for a Vulos deployment: `GET /api/ai/status` on the OS, and confirm `LLMUX_URL`/`LLMUX_KEY` in the OS backend environment. Errors are always normalized to the OpenAI shape (`{"error":{"message","type","code"}}`) with a faithful status — the `code` strings in this page are what to grep for.

## Reporting a problem

Every request is access-logged with a `request_id` (plus method, path, status and duration — never message content). When filing an issue, include the `request_id` from the gateway log, the error body your client received, the provider `type` involved, and the sovereignty tier shown for that provider in the master-key `/health` output. For suspected vulnerabilities, follow [SECURITY.md](../SECURITY.md) — report privately, never in a public issue.

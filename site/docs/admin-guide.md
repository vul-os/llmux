# llmux Admin Guide

This guide is for the operator running an llmux gateway — for a household box, a team, or a fleet billed through the Vulos control plane. It covers the money controls (per-key budgets, rate limits, and the managed-credit behaviour when a control plane gates central spend), how usage is metered and reported, how model routing and selection actually resolve, and precisely what llmux logs and forwards — so you can state your privacy posture with confidence. Everything here is grounded in the shipped code; exact error codes and defaults are called out so you can recognise them in the wild.

## The admin surface

All admin endpoints require the **master key** (`LLMUX_MASTER_KEY`) — virtual keys are never accepted there. On a keyless dev box, admin routes are reachable from loopback only.

| Endpoint | Purpose |
|---|---|
| `GET /admin/keys` | Virtual keys with budgets, spend, RPM |
| `GET /admin/usage` | Usage by model |
| `GET /admin/byok/{account}` | Provider names an account has BYOK keys for (never the key values) |
| `PUT /admin/byok/{account}/{provider}` | Set an account's own provider key (that provider goes BYOK/unmetered for them) |
| `DELETE /admin/byok/{account}/{provider}` | Revert the account to central keys (metered) |
| `GET /metrics` | Prometheus metrics |
| `GET /health` | Liveness — plus, for the master key (or loopback on a keyless box), the full provider/sovereignty posture |

Quick reference:

```bash
export AUTH="Authorization: Bearer $LLMUX_MASTER_KEY"
curl -s -H "$AUTH" http://localhost:4000/admin/keys      # budgets, spend, rpm
curl -s -H "$AUTH" http://localhost:4000/admin/usage     # usage by model
curl -s -H "$AUTH" http://localhost:4000/health          # provider tiers + egress posture
curl -s -X PUT -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"api_key":"sk-their-own-key"}' \
  http://localhost:4000/admin/byok/acct_42/openai        # account goes BYOK for openai
curl -s -X DELETE -H "$AUTH" \
  http://localhost:4000/admin/byok/acct_42/openai        # revert to central (metered)
```

The same data is visualised in the embedded dashboard at `/ui` (usage by model, key budgets and spend, the live model catalog). The dashboard's static pages are public; its admin views call the master-key-gated `/admin/*` API. The CLI mirrors it without HTTP:

```bash
./dist/llmux keys       # virtual keys: budget, spend, rpm
./dist/llmux models     # models with pricing + context window
./dist/llmux catalog    # price catalog count and last sync time
```

## Budgets and limits

### Per-key controls (standalone)

Each virtual key in `keys[]` carries three independent controls:

```jsonc
{
  "keys": [
    {
      "key": "sk-team-a",
      "name": "team-a",
      "budget_usd": 25.0,                        // 0 = unlimited
      "rpm": 60,                                  // 0 = unlimited
      "allowed_models": ["assistant", "cheapest"] // empty = all models
    }
  ]
}
```

| Field | Meaning | When exceeded |
|---|---|---|
| `budget_usd` | Lifetime USD spend cap (0 = unlimited) | HTTP **402**, `type: "insufficient_quota"`, `code: "budget_exceeded"`, message `budget exceeded for key <name>` |
| `rpm` | Requests per minute, token-bucket (0 = unlimited) | HTTP **429**, `code: "rate_limit_exceeded"`, message `rate limit exceeded for key <name>` |
| `allowed_models` | Model allow-list (empty = all) | HTTP **403**, `code: "model_not_allowed"`, message `model <m> not allowed for this key` |

How enforcement behaves:

- Budget checks are conservative under concurrency: each in-flight request holds a small reservation ($0.05) against the budget, so parallel requests can't collectively overshoot; the reservation settles against real cost when the response is metered.
- The RPM bucket has capacity = `rpm` and refills at `rpm/60` per second — short bursts up to the full minute allowance are allowed.
- Spend persists to `key_store_path` (JSON file) or to Postgres when a DSN is configured (`key_store_path` is ignored then).
- Key tokens are stored as SHA-256 hashes — in Postgres, and in the Redis rate-limit keys — so a database dump never yields live credentials.

Global request guards, independent of keys:

- Request bodies are capped at **32 MiB**.
- `upstream_timeout_seconds` adds a per-request deadline for non-streaming calls (streaming has none by design).
- `max_response_bytes` bounds non-streaming upstream response bodies (0 = unlimited).
- `drop_params[]` strips named request fields before forwarding to OpenAI-shaped upstreams.

### Managed credit via the control plane

With the CP seam wired (`LLMUX_CP_URL` + `LLMUX_CP_SECRET`), budget gating becomes **account-centric**: before dispatch, llmux resolves the request's bearer to an account and checks that account's entitlement from the control plane:

```
GET {cp}/api/entitlements?product=llm&account_id=…
→ { "llm_enabled": …, "llm_budget_usd": …, "suspended": … }
```

This is how a "managed credit" allowance (for example the Vulos free tier's small `llm_budget_usd`) is enforced:

- **Denied** when the account is suspended, LLM access is disabled, or the remaining budget is ≤ 0 → HTTP **402** `insufficient_quota` / `budget_exceeded`, with reason `"account suspended"`, `"llm not enabled for account"` or `"llm budget exhausted"`. Budget denial is **402, not 403** — 403 is the sovereignty gate or a model allow-list.
- An in-flight reservation again bounds concurrent overshoot, so a burst can't blow materially past the credit.
- Entitlements are cached briefly — `cp_entitlement_ttl_seconds`, default 30 s. This is a config-file key only; it has **no** env-var binding.
- **BYOK requests bypass the central budget entirely** — an account calling with its own provider key neither consumes nor is blocked by its managed credit. BYOK is the natural "unlimited lane"; see [LLM-ACCESS.md](./LLM-ACCESS.md).

The full `cp` config block, with its exact JSON keys:

```jsonc
{
  "cp": {
    "cp_url": "https://control-plane.example.com",
    "cp_shared_secret": "…",              // sent as X-Relay-Auth
    "cp_rpm": 120,                         // per-account RPM while the CP is healthy
    "cp_entitlement_ttl_seconds": 30,      // entitlement cache TTL
    "cp_degraded_rpm": 20,                 // per-account RPM while the CP is DOWN
    "cp_degraded_fail_open": false         // true = allow unmetered during outage
  }
}
```

### Degraded mode (CP unreachable)

llmux fails *safe*, not open, when the control plane is down:

| Setting (config / env) | Effect |
|---|---|
| `cp_degraded_rpm` / `LLMUX_CP_DEGRADED_RPM` | Per-account requests/minute allowed while the CP is unreachable (default **20**/min) |
| `cp_degraded_fail_open` / `LLMUX_CP_DEGRADED_FAIL_OPEN` | `true` ⇒ allow requests through unmetered instead — only if you accept the spend risk |
| `cp_rpm` / `LLMUX_CP_RPM` | Ordinary per-account RPM cap while the CP is healthy |

The degradation ladder, in order:

1. Transient CP error with a warm cache → llmux reuses the **last-known-good entitlement** for the account; traffic is unaffected.
2. Cold cache and the CP is down → the account is bounded by the degraded rate cap; denials are 429 with reason `"control plane unavailable; degraded rate limit for account <id>"`.
3. `cp_degraded_fail_open: true` → requests pass unbounded and unmetered until the CP returns. Use only where availability outweighs spend control.

## Metering and usage reporting

Every served request produces one usage record with exactly these fields:

```
id, time, key, account_id, model, stream,
prompt_tokens, completion_tokens, total_tokens,
cost_usd, cached, byok
```

**No prompt or completion text is ever part of a usage record.**

Records flow to up to three sinks:

1. **The in-process store** — powers `GET /admin/usage`, the dashboard's usage view, and `/metrics`.
2. **A local JSONL ledger** (optional) — set `LLMUX_USAGE_LOG=/var/lib/llmux/usage.jsonl` and every record is appended as one JSON line (file created mode `0600`). This is env-only; there is deliberately no config-file key for it.
3. **The control plane** (optional) — with the CP seam wired, each finalized **central** (non-BYOK) request emits:

```
POST {cp}/api/usage
Idempotency-Key: <usage record id>
X-Relay-Auth: <shared secret>

{ "idempotency_key": "…", "product": "llm", "account_id": "…",
  "kind": "llm_tokens", "count": <total tokens>, "cost_usd": <USD> }
```

Points worth internalising:

- The CP dedupes on `Idempotency-Key`, so retries bill at most once.
- Note what is *absent* from the CP payload: no prompts, no completions, not even the model name — only the account id, token count and cost.
- **BYOK records are never POSTed to the CP.** They carry `byok: true` and are dropped before the billing sink, while the local ledger and dashboard still record them so the account keeps visibility into its own unbilled usage.
- **Durability caveat:** CP delivery uses a bounded in-memory retry queue (depth 1024, 5 attempts per record). If the CP stays down past that, or the process crashes with records queued, those records survive only in the JSONL ledger — there is no automatic reconciler yet. If billing completeness matters, always configure `LLMUX_USAGE_LOG` and retain the file.

### Cost accounting

Per-request cost comes from the pricing catalog:

- A built-in seed catalog means cost accounting works fully offline.
- Live syncs merge prices from OpenRouter and LiteLLM (`pricing.sources`; interval `sync_interval_minutes`, default 360 — or env `LLMUX_SYNC_INTERVAL_MIN`).
- Individual models can be pinned with `pricing.overrides` (`{provider, input_per_mtok, output_per_mtok, context_window, max_output, capabilities}`).
- Cost appears in each response's `usage` block; the merged catalog is served at `GET /v1/catalog.json` and via `llmux catalog`.

Streaming responses are metered too: llmux forces a final usage chunk from the upstream (it injects `stream_options.include_usage` itself), and if the upstream still omits usage it falls back to a ~4-characters-per-token estimate — a stream is never billed as zero. For the forwarded modality routes, up to 1 MiB of the response is tapped to parse usage; larger streams are estimated from served bytes.

## Model routing and selection

How a request's `model` string resolves, in order:

1. **Exact route match** — a route whose `model` equals the requested string (an alias: `"model": "assistant"` → provider `local`, `target_model` `llama3`).
2. **Wildcard prefix** — the longest matching trailing-`*` pattern (e.g. `"claude-*"` → provider `anthropic`). An empty `target_model` forwards the requested name; a `target_model` containing `*` substitutes the matched remainder.
3. **Catch-all** — a `"model": "*"` route, forwarding the requested name unchanged.
4. **`provider/model` prefix** — if nothing above matched and the segment before `/` names a configured provider, route the remainder there (`openai/gpt-4o`).
5. Otherwise: HTTP 404, `code: "model_not_found"`.

**Least-cost selection.** A route with `"strategy": "least-cost"` and a `candidates[]` list picks the cheapest candidate by catalog price (input+output per MTok) at request time; the remaining candidates become its fallback chain in cost order. Candidates with unknown pricing sort last. The route's `model` name (e.g. `"cheapest"`) is operator-chosen, not a reserved word — only the `"least-cost"` strategy string is.

**Fallbacks and retries.**

- Each route may list `fallbacks` (provider names), tried in order after the primary.
- Per target, llmux retries up to `retry.max_retries` (default 2) with exponential backoff (`backoff_ms` default 200 → 200/400/800…) on retryable failures: HTTP **429, 500, 502, 503, 504** or transport-level errors.
- Other client errors (4xx) return immediately — no retry, no failover.
- Sovereignty-blocked targets are skipped without being dialed, so a local fallback can still serve; if *every* target is blocked, the 403 surfaces to the caller.
- For streams, failover only happens before the first byte reaches the client; a mid-stream failure ends the stream and meters what was served.

**Adapter maturity.** Stability is disclosed per provider in `/health` and the startup logs: `passthrough` is `stable`; `anthropic`, `gemini`, `azure` are `beta`; `cohere`, `bedrock` are `experimental`. Only `passthrough` providers serve the forwarded modality routes (`/v1/completions`, `/v1/responses`, images, audio, moderations, rerank) — translating adapters return 501 there.

## Response caching

The response cache is a spend and latency control, so it belongs in the admin picture:

```jsonc
{
  "cache": {
    "enabled": true,
    "ttl_seconds": 300,            // 0 = no expiry
    "max_entries": 10000,          // 0 → default 10000 (LRU)
    "semantic": false,             // embedding-similarity matching
    "embedding_model": "…",        // used by the semantic cache
    "similarity_threshold": 0.95   // 0 → default 0.95
  }
}
```

- Caches are **scoped per virtual key** — one key's cached completions are never served to another key. A "missed" cache across two keys is correct behaviour, not a bug.
- Cache hits are marked `cached: true` on the usage record; the metering decision is attributed to the route's primary provider's BYOK status (no provider call is made).
- The semantic cache's embedder passes through the same sovereignty gate as every other dispatch — a semantic cache configured against a remote embedding model will be blocked unless that provider may egress.
- With `LLMUX_REDIS` set, the cache is shared across replicas; otherwise it is in-memory per process.

## Logging and privacy posture

What llmux writes, in full:

- **Access log** (structured slog): `request_id`, `method`, `path`, `status`, `dur_ms` per request. `/health` and `/metrics` are not access-logged. **Prompt and completion content never appears in any log line.**
- **Usage records**: the token/cost fields listed above — no message content.
- **Sovereignty events**: every *blocked* egress attempt is logged and counted in the `llmux_egress_blocked_total` metric, and every *permitted* off-box call is logged with its tier — off-box traffic is always observable, never silent.
- **Secrets**: virtual-key tokens are hashed (SHA-256) at rest; BYOK provider keys are AES-256-GCM-encrypted under your KEK, never logged, and never returned by any endpoint (the BYOK store is write-only from the outside; even the in-memory copy holds ciphertext).

What llmux forwards:

- **To providers**: the request itself (that's the job) — but only to providers the sovereignty gate permits, with the correct per-account key. Key resolution is re-evaluated per fallback target, so an account's OpenAI key is never sent to Anthropic.
- **To the control plane**: billing counts only (`account_id`, token count, cost). Never prompts, never completions.
- **To no one else**: llmux has no telemetry.

### Auditing the posture

`GET /health` is reachable without auth but returns only `{"status":"ok"}` to anonymous callers. With the master key — or from loopback on a keyless box — it additionally discloses, per provider: `name`, `type`, `stability`, `tier`, `tier_label`, `locality`, `egress_allowed`, plus a sovereignty summary (`default: "local"`, providers grouped by tier, and the list of egress-allowed names). Re-check it after every provider/config change:

```bash
curl -s -H "Authorization: Bearer $LLMUX_MASTER_KEY" http://localhost:4000/health | jq .sovereignty
```

## Operational checklist

- Set `LLMUX_MASTER_KEY` before exposing the gateway beyond loopback. Keyless non-loopback binds are refused unless you force `LLMUX_INSECURE_KEYLESS=1` — don't; and note that keyless-on-loopback leaves `/admin` and `/metrics` open to local callers.
- Give every application its own virtual key with a budget and RPM — never hand out the master key. In the Vulos suite, that means minting a dedicated key for the OS's `LLMUX_KEY`.
- Set `LLMUX_USAGE_LOG` if you bill through the CP (crash-durable ledger) or simply want an audit trail.
- Watch `/metrics` (master key): request counts, latencies, and especially `llmux_egress_blocked_total` — a rising count means something is repeatedly trying to reach a provider you haven't opted in.
- Keep off-box opt-ins narrow: prefer explicit `"tier": "sovereign"`/`"brokered"` declarations for endpoints you actually vouch for over the broad `"allow_egress": true`.
- If you enable BYOK (`LLMUX_BYOK_KEK`), also set a master key when listening on TCP — startup warns loudly otherwise, because anyone reaching the port could set or clear account keys.
- Add Postgres + Redis only when you run multiple replicas; single-replica in-memory is fully supported.
- Treat `cohere`/`bedrock` routes as experimental and `anthropic`/`gemini`/`azure` as beta when planning fallback chains; keep a `stable` passthrough (or local) target in every chain.

## See also

- [GETTING-STARTED.md](./GETTING-STARTED.md) — deployment, providers, and Vulos OS wiring.
- [TROUBLESHOOTING.md](./TROUBLESHOOTING.md) — symptom-by-symptom fixes.
- [LLM-ACCESS.md](./LLM-ACCESS.md) — BYOK vs central, the product contract, free-tier hook.
- [control-plane.md](./control-plane.md) — the CP seam in brief.
- [HARDENING.md](../HARDENING.md) — full production security posture.

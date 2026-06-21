# Production hardening checklist

Tracks the work to make llmux a trustworthy production gateway (the "real
product / LiteLLM competitor" bar). `[x]` done · `[~]` partial · `[ ]` todo.

## Correctness verification (the gate)
- [~] Conformance harness: record/replay/live fixture transport (`core/conformance`).
- [ ] Standard battery per provider: non-stream, stream, tools, vision, errors, usage.
- [ ] Golden fixtures recorded from real APIs (`make record`, needs keys).
- [ ] Live smoke suite (`make smoke`, gated on `LLMUX_LIVE=1` + keys) in CI.
- [ ] Real end-to-end proof: OpenAI + Anthropic + Gemini through the gateway.

## Request lifecycle
- [x] Client disconnect cancels the upstream request (context propagation verified).
- [x] Configurable non-streaming upstream timeout (`upstream_timeout_seconds`).
- [x] Response size bound for non-streaming bodies (`max_response_bytes`, all providers).
- [x] Streaming: mid-stream failure surfaces a trailing SSE error event (no hang).
- [x] Retry idempotency: spend recorded once after success, not per attempt.

## Fidelity to the OpenAI contract
- [x] Pass through `x-ratelimit-*` and `retry-after` response headers.
- [x] Error taxonomy: upstream status + structured error type relayed faithfully.
- [ ] `n > 1`, `logprobs`, `response_format`/structured outputs, `parallel_tool_calls` (passthrough forwards raw; adapters: verify).
- [ ] Token accounting when upstream omits usage (estimate + flag as estimated).

## Security (H5 — done; see SECURITY.md)
- [~] SSRF: confirmed not client-reachable (operator-config only); base_url allowlist deferred to multi-tenant admin.
- [x] No header/secret injection (Go rejects CRLF; config-only headers).
- [x] Secret redaction: keys never logged; internal/transport detail not echoed to clients.
- [x] Admin/metrics: master-key only, constant-time compare; `/health` disclosure gated.
- [x] Per-key cache isolation; Bedrock path escaped; response size bounded.

## Scale & ops (H6 — done)
- [x] Postgres-backed keys/spend/budgets (correct across replicas) — verified vs local PG18.
- [x] Redis-backed rate limits + response cache (correct across replicas) — verified vs local Redis.
- [ ] Graceful drain on shutdown (in-flight streams finish).
- [ ] Structured logs + request-id already in place; add upstream latency + provider labels to metrics.

## Done
- [x] Honest stability tiers + SUPPORT matrix + experimental warnings (H1).
- [x] Structured access logs + `X-Request-ID` propagation.
- [x] Prometheus `/metrics`, admin usage/keys, persistent spend (single-instance).

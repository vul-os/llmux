# Security

## Reporting
Email security@llmux.to (or open a private advisory). Please do not file public
issues for vulnerabilities.

## Threat model
llmux is a gateway: it holds provider API keys, makes outbound HTTP to
operator-configured upstreams, exposes user (`/v1/*`) and admin (`/admin/*`)
endpoints, and serves a web UI (`/ui`). Clients are semi-trusted (authenticated
by virtual key or master key); operators are trusted (they set provider configs).

## Posture (after the H5 review)

Fixed:
- **Constant-time master-key compare** (`crypto/subtle`) for `/admin`, `/metrics`,
  and the API master key — no timing oracle.
- **No internal detail in error responses.** Transport errors (which contain the
  outbound URL/host) and non-JSON upstream bodies are never echoed to clients;
  they return a generic message and the detail is logged server-side. Structured
  upstream error *messages* are still relayed (useful + safe).
- **`/metrics` requires the master key**; **`/health`** discloses the provider
  list only to the master key (status-only otherwise).
- **Response size bound** (`max_response_bytes`) applied to all non-streaming
  upstream decodes (passthrough + all adapters), preventing memory exhaustion.
- **Per-key cache isolation.** Cache keys are scoped by the calling virtual key,
  so tenants never share cached responses.
- **Bedrock model id path-escaped** (closes a path-shaping vector).
- **Embeddings model allow-list** — `/v1/embeddings` now enforces the per-key
  model allow-list (it previously bypassed it; a restricted key could embed any
  model). Test: `TestEmbeddingsAllowListEnforced`.

## Dependency & toolchain scanning
- `govulncheck` runs in CI. The H5 pass found 9 advisories, **all in the Go
  standard library** (DoS in net/http HTTP/2, crypto/tls, crypto/x509, net/url) —
  cleared by pinning the build toolchain to **go1.25.11** (`toolchain` in go.mod).
  No third-party dependency had a called vulnerability.
- A dedicated security test suite (`core/server/security_hardening_test.go`)
  covers: auth matrix, budget/rate-limit enforcement, allow-list bypass on chat +
  modality + embeddings routes, secret/host non-leakage, raw-body non-echo,
  oversize-body handling, SSRF/host-control, and CRLF/header-injection.

Verified safe:
- Client `Authorization` is **never** forwarded upstream — each adapter sets its
  own provider credential.
- No client-controlled host/scheme in outbound URLs (model affects path only;
  Gemini & Bedrock paths are escaped). SSRF requires malicious *operator* config.
- API keys are never logged or returned; `/admin/keys` masks tokens;
  `config.String()` is redaction-safe.
- Request body capped (32 MiB); SSE lines capped (1 MiB); rate-limit/spend maps
  only grow for known keys; caches are size-bounded with eviction + TTL.
- Panics are recovered without leaking stack traces; unix socket is `0600`.

## Known / roadmap
- **SSRF base_url allowlist:** upstream URLs are operator-configured today. If a
  future multi-tenant admin API lets less-trusted users set `base_url`, add an
  allowlist blocking internal/link-local/metadata ranges (169.254.169.254, etc.).
  Tracked in HARDENING.md.
- Spend is not charged on cache hits (by design); revisit for billing integrity
  alongside multi-tenant Cloud.

# Changelog

All notable changes to `llmux` are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

### Added
- **Structural egress guards** (`core/sovereign/egress_guard_test.go`) — the
  sovereignty gate is now enforced by CI rather than by discipline. Three tests
  read the source instead of running it: every call to an egress-capable
  provider method must sit in a function that also calls `enforceSovereignty`
  (the egress-method set is derived from the `Provider`/`Forwarder` interfaces,
  so a new verb is covered automatically); every file that can open an outbound
  connection must be declared in a registry with a written reason; and
  `core/server` may not build outbound HTTP requests at all. Each asserts a
  coverage floor so a broken scan fails instead of passing vacuously.
- **No-account guards** (`core/server/no_login_surface_test.go`) — login-shaped
  paths must not answer, no response may set a cookie, and `core/server` may not
  reference cookies. llmux is configured by endpoint and authenticated by
  bearer token; this keeps it that way.
- **API-doc guards** (`core/server/api_docs_test.go`) — `docs/api.md` must
  document every served route, and every documented route must answer with the
  credential the page names. Consumers write clients from that page without
  reading the source.
- **`site/docs` is generated** from the canonical `docs/` tree (`make site-docs`,
  `site/gen`), with links rewritten so they resolve inside the site bundle.
  `TestSiteDocsInSync` fails on drift; this also fixed 24 dead links that could
  never have resolved in the shipped bundle.

### Changed
- **Gated integration tests now skip loudly**: a run without Postgres/Redis
  prints which tests were skipped *and which properties went unverified*, the
  gated corpus size is checked against the source, and CI sets
  `LLMUX_REQUIRE_INTEGRATION=1` so a skip there is a failure rather than a green
  run with no cross-replica coverage.
- Documented what the sovereignty gate does **not** cover (price-catalog sync,
  control-plane seam, Redis, Postgres) in `docs/architecture.md`, plus a short
  copyable description of the pattern. The price-catalog sync is on by default
  and is now disclosed as such in the README and `docs/configuration.md`.

### Fixed
- `"pricing": {"sources": []}` in a config file now actually disables the price
  feeds. The merge used a `len() > 0` check, so an explicitly-empty list was
  indistinguishable from an absent one and the shipped defaults kept dialing —
  the one outbound call a stock gateway makes could not be turned off.
- `TestPGStoreUsesDedicatedSchema` no longer depends on the database's history:
  it cleans `public.llmux_keys` too, so its "did not leak into public" assertion
  means "this run did not leak" instead of failing forever on a dev machine that
  once held such a table.
- `ee/README.md` described SSO/RBAC/audit features, a `cloud/` directory, and an
  MIT-only license — none of which exist. Replaced with an accurate statement:
  the directory is empty and reserved.

## [0.2.0] - 2026-07-17

### Added
- **Sovereignty gate** — inference runs on your box by default. A default-deny
  egress policy (`core/sovereign`) gates every dispatch path (unary + streaming
  chat, embeddings, the semantic-cache embedder, and all modality/forward
  routes) before any network call. Off-box providers are blocked unless
  explicitly opted in per provider (`allow_egress`, `allow_brokered`, or
  `"tier": "sovereign"`). Evolved into a 4-tier "where your AI runs" model
  (local / sovereign / brokered / external); fails closed; permitted off-box
  calls are logged with their tier; `/health` and the startup log disclose the
  posture. Documented in README, `docs/architecture.md`,
  `docs/configuration.md`, and `SECURITY.md`.
- **Postgres seam standardized** on a shared `DATABASE_URL` (or
  `VULOS_DATABASE_URL`) with a dedicated `llmux` schema, so llmux can share one
  Postgres instance (e.g. a single Neon database) with the rest of the suite
  without colliding with other products' tables.
- **Audio endpoints** — `/v1/audio/transcriptions` and
  `/v1/audio/translations` (multipart), metered and gated by the sovereignty
  policy like every other route.
- **Durable CP usage spooling** — usage records are written to an optional
  on-disk spool before being handed to the fast-path retry queue, and a
  background reconciler re-delivers anything the control plane hasn't
  acknowledged. Closes the gap where an extended CP outage, a full retry
  queue, or a process crash could silently drop a billing record.
- Comprehensive product manual: `docs/GETTING-STARTED.md`,
  `docs/ADMIN-GUIDE.md`, `docs/TROUBLESHOOTING.md`.
- Third-party license notices: bundled `@license` banners are preserved in the
  web build and a third-party notices page is generated and surfaced.
- Web frontend test layer: Playwright E2E (boot guard, dashboard, docs) and
  Vitest unit/component tests, including an adversarial-security test pass.

### Changed
- Relabeled the "sovereign" and "brokered" tiers to be honest about what
  they are: operator-declared, unverified endpoints — not a Vulos-operated or
  Vulos-vetted guarantee. Enforcement and tier keys are unchanged.
- README and docs made self-contained (dropped the "Part of VulOS" suite-map
  banner in favor of a plain logo footer) and de-genericized: stale
  `@vulos.net` references purged, the mail connector's `[ai]` block reframed as
  the connector's own `[ai]` block, sibling products named correctly (Diwan and
  lilmail), and a stale mail link dropped.
- Documented previously-implicit behavior: modality routes are
  passthrough-only (translating adapters return 501), `model_not_priced`
  fail-closed budgeting, and the `/health` auth surface reconciled between
  `api.md` and `architecture.md`.
- Go toolchain bumped to go1.25.12; web tooling upgraded (Vite 5→8,
  `@vitejs/plugin-react` 4→6) to clear reachable stdlib and dev-tooling
  advisories.
- Removed the unused `requestIDFrom` server helper.

### Fixed
- **Virtual-key tokens hashed at rest** in Postgres and Redis (SHA-256), so a
  Postgres dump or a Redis `SCAN`/`MONITOR` can no longer harvest live bearer
  credentials.
- **Sovereignty gate bypasses closed**: `handleForward` (`/v1/completions`,
  `/responses`, `/images`, `/audio`, `/moderations`, `/rerank`) never called
  the egress gate, letting 6 routes reach a blocked remote provider; the
  semantic-cache embedder called the provider directly on every prompt,
  bypassing the gate entirely (a blocked embed model is now treated as a
  cache miss instead of an egress path).
- **Fail-closed hardening**: an unset `LLMUX_MASTER_KEY` left the gateway an
  open proxy with `/admin` and `/metrics` reachable from anywhere; a keyless
  gateway now refuses to bind on a non-loopback address (opt out via
  `LLMUX_INSECURE_KEYLESS`), and keyless `/admin`/`/metrics`/`/health` are
  loopback-only.
- **Fail-closed billing**: a budgeted key routed to a routable-but-unpriced
  model was previously allowed through unbilled; now denied.
- **Fail-closed keys**: a Postgres or Redis outage during key lookup was
  previously treated as an allow; now denied.
- Gemini tool-parameter schemas containing `$ref`/`$defs` (the routine output
  of pydantic/zod schema generators) are now inlined before being sent to
  Gemini, which rejected dangling `$ref` pointers with HTTP 400.
- Config keys that were only bindable from one side: `cp_entitlement_ttl_seconds`
  gained the `LLMUX_CP_ENTITLEMENT_TTL_SECONDS` env var, and `LLMUX_USAGE_LOG`
  gained a config-file counterpart.
- Cache-hit requests are now asserted, under test, to never be billed twice.

## [0.1.0] - 2026-06-28

Initial release.

[Unreleased]: https://github.com/vul-os/llmux/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/vul-os/llmux/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/vul-os/llmux/releases/tag/v0.1.0

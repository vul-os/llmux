# Changelog

All notable changes to `llmux` are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

No unreleased changes.

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
  `@vulos.net` references purged, "Vulos Mail [ai] block" reframed as the mail
  connector's own `[ai]` block, "Vulos Office" corrected to "Ofisi", and a
  stale Vulos Mail link dropped.
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

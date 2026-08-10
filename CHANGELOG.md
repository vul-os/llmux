# Changelog

All notable changes to `llmux` are documented here.

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [Unreleased]

## [0.1.5] - 2026-08-10

A security release. Two changes are **breaking for embedders** — both correct
the same mistake, that a library was reading its host's environment.

### Breaking

- **The configuration document now wins over the environment.** `applyEnv` ran
  *after* the caller's document and overrode it, so an explicit config could be
  silently replaced by whatever the host had exported.
- **Library mode no longer reads `DATABASE_URL` or `VULOS_DATABASE_URL`.** Those
  are the host application's variable names; in library mode the environment
  belongs to the host, not to llmux. `LLMUX_POSTGRES` is namespaced, so
  honouring it can only be intentional, and it is still honoured. The sidecar
  (`config.Load`) is unchanged.
- **`llmux_close` now blocks**, up to a 5 s grace, and must not be called from
  inside a chunk callback. The bound is deliberate: `llmux_close` is `void`, so
  an unbounded drain would deadlock exactly that case.

### Security

- **`llmux_new` adopted the host's `DATABASE_URL` and ran DDL against it.** A
  Rails or Django application with `DATABASE_URL` set that loaded `libllmux`
  with an explicit provider-only config got `CREATE SCHEMA` and `CREATE TABLE`
  executed in its production database — while `ffi/include/llmux.h` promised
  inertness "unless **your configuration** names a Postgres DSN". Fixed by the
  two precedence changes above.
- **A Go panic at any `//export` killed the host process.** In `c-shared` mode a
  panic does not unwind into C. The HTTP shell has had this backstop since
  before v0.1.0, so the same bug was a logged 500 as a sidecar and a dead uWSGI
  or JVM worker as a library — the two modes the README presents as
  interchangeable. All seven exports and the four pure-Go entry points now
  recover, **including panics thrown by the host's own chunk callback**, which
  runs inside the stream call frame.

### Added

- **`llmux_cancel(uint64_t h)`** — the ABI's seventh symbol. Previously the only
  way out of a blocked call was `llmux_close`, which destroys the gateway and
  every other stream on it. No SDK binds it yet.
- **Liveness bounds on streaming**: 60 s to first byte, 120 s idle, re-armed per
  chunk. Deliberately not a wall-clock deadline — that cannot distinguish a long
  generation from a dead connection, which is why there was none.

### Fixed

- `Gateway.Close` wrote `g.rdb = nil` unsynchronised — a data race under `-race`.
- `*err` is now cleared on entry, so a binding reusing one `err` across calls
  cannot see a previous failure's string and double-free it.
- The Postgres connect is bounded rather than `context.Background()`.

## [0.1.4] - 2026-08-10

### Fixed

- **v0.1.3 shipped a library that misreported its own version.** `VERSION` was
  bumped to 0.1.3 and `ffi/abi.go` was left at 0.1.2, so `llmux_abi_version()`
  answered `0.1.2` from a 0.1.3 build — breaking the stale-library detection
  that symbol exists to provide, in the direction that calls a current library
  old. `make test-ffi` was red on `main` from the moment 0.1.3 was tagged.

  Every check run before tagging was green, because **`ffi/` is a separate Go
  module and `go test ./...` at the root cannot reach it.**
  `TestFFIABIVersionTracksTheVERSIONFile` now runs in the root module and reads
  `ffi/abi.go` as text, since importing across that boundary is precisely what
  is impossible. openrate shipped the identical defect in the same release, from
  the same cause, and now carries the same guard.
- **`ffi/README.md` and `sdks/rust/README.md` claimed Go installs a `SIGPROF`
  handler.** It does not. `sdks/java/signal-probe.sh` measures the five that are
  replaced — `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPIPE`, `SIGURG` — and confirms
  JFR profiling is unaffected (428 execution samples collected with the library
  loaded). Documenting a hazard that does not exist invites defensive code
  around a non-problem.

## [0.1.3] - 2026-08-09

Additive. Nothing that compiled against v0.1.2 changes behaviour; the breaking
changes were all in that release.

### Added

- **A C ABI, so languages other than Go can embed llmux in-process.**
  `ffi/` is a separate Go module (`github.com/vul-os/llmux-ffi`, named outside
  the parent import path so Go's `internal/` rule constrains it exactly as it
  would a third-party embedder). Six symbols — `llmux_new`, `llmux_call`,
  `llmux_stream`, `llmux_close`, `llmux_free`, `llmux_abi_version` — carrying the
  same OpenAI-shaped JSON the HTTP API already serves, so there is one wire
  contract rather than two. Handles are `uint64` registry keys that are never
  reused, so use-after-close is a readable error and not a segfault.
  A C smoke test dlopens the built library, runs 32 named checks, and then
  asserts that 32 checks ran.
- **Fifteen language packages**, each with direct (in-process) and sidecar
  modes: go, c, cpp, rust, swift, deno, bun, node, python, java, kotlin, dotnet,
  ruby, php, elixir. Indexed in [`sdks/README.md`](sdks/README.md).
- **Five documentation pages** — `choosing-a-mode`, `embedding`, `c-abi`,
  `sdks`, `quickstarts` — taking the set from 12 to 17.
- **The admin console can be compiled out** with `-tags noui` (33,312 bytes), and
  a program importing only `core/gateway` links zero console bytes regardless.

### Fixed

- **The darwin dylib had a bare install name**, so `dyld` never consulted an
  executable's `-rpath` and any consumer that *linked* llmux — rather than
  `dlopen`ing it by path — died at startup with `Library not loaded`. Nothing
  caught it because every test and example dlopens by absolute path, the one
  case that works either way. Now stamped `@rpath/`.
- **`docs/client-examples.md` claimed the Go SDK "runs the gateway in-process —
  imports `core/server` directly, no subprocess".** It bound a loopback TCP
  listener and polled `/health`. There is now a real in-process path; the
  loopback shim is documented as the deprecated compatibility path it is.
- **Cancelling a stream did not always stop the upstream call.** A consumer
  taking 3 of 10 chunks still caused all 10 to be generated and metered in the
  Bun binding (and the same shape was found in Kotlin and C#). Each README now
  states its measured callback count under early exit.

### Notes

- Prebuilt FFI libraries exist for **darwin/arm64** and **linux/arm64** only;
  linux/amd64 is CI-only, and **no Windows DLL ships**. openrate's matrix
  differs — do not read one off the other.
- Loading the library replaces five of HotSpot's signal handlers (`SIGSEGV`,
  `SIGBUS`, `SIGFPE`, `SIGPIPE`, `SIGURG`) and adds `SA_ONSTACK` to three more.
  `SIGPROF` is **not** touched, so JFR profiling is unaffected. `libjsig` fixes
  the rest but is a java launch flag, which is why the sidecar is the
  recommended default for Java and Kotlin. Evidence:
  [`sdks/java/signal-probe.sh`](sdks/java/signal-probe.sh).
- The Go runtime is **not fork-safe**, and the failure is a false green:
  `models` succeeds in a forked child while `chat` hangs.

## [0.1.2] - 2026-08-09

llmux is now an importable Go library. The gateway logic moved into
`core/gateway`, which speaks in Go types and never touches `net/http` server
machinery; `core/server` is a thin HTTP shell over it, and the admin console can
be compiled out entirely.

**This release breaks exported API despite being a patch version.** It stays on
the 0.1 line because the blast radius is provably nil: the old module path
`github.com/llmux/llmux` pointed at a GitHub organisation with no repositories,
so it never resolved and nothing could have depended on it. Read the Breaking
section before upgrading anyway.

### Breaking

- **The module path is now `github.com/vul-os/llmux`.** It was
  `github.com/llmux/llmux`, which never resolved — the code has always lived at
  `vul-os/llmux`. Update imports; there is no compatibility shim, because there
  is nothing to be compatible with.
- **Four exported package variables were removed** from `core/provider`:
  `DropParams` and `MaxResponseBytes` (`response.go`), and `DefaultHTTPClient`
  and `StreamHTTPClient` (`provider.go`). They are now fields on the new
  `provider.Options`, passed per instance. They were process-global and
  `server.New` mutated the first two, so two gateways in one process silently
  corrupted each other's limits — the reason this had to change for embedding to
  be safe at all. The identically-named `config.Config` fields are unaffected.
- **`providers.Build` takes three arguments**: `Build(cfgs)` →
  `Build(cfgs, opts provider.Options, log *slog.Logger)`.
- **Every provider adapter's `New` takes `provider.Options`**: `New(c)` →
  `New(c, opts)`, across `passthrough`, `anthropic`, `gemini`, `cohere`,
  `bedrock` and `azure`.

### Added

- **`core/gateway` — the in-process API.** `New`, `Chat`, `ChatStream`, `Embed`,
  `Models`, `Authorize`, `Run`, `Start`, `Close`, and a `Result` carrying the
  response alongside the provider that actually served it after failover, the
  BYOK flag, the cache-hit flag and the upstream rate-limit headers. Streaming
  is a `ChunkFunc` callback, not an SSE writer. No `http.Request` or
  `http.ResponseWriter` appears anywhere in the surface.
- **`gateway.New` starts no goroutines and opens no sockets.** Background work —
  the pricing syncer, the key-store spend flusher — begins only when the caller
  invokes `Run`. Previously the pricing syncer was on by default and reached out
  to openrouter.ai and raw.githubusercontent.com every six hours, which is a
  surprise inside someone else's process. Two exceptions are documented rather
  than papered over: `New` connects and migrates eagerly when `cfg.Postgres` is
  set, and it reads `os.Getenv` for any provider configured with `api_key_env`
  (config-directed, not auto-detected).
- **The admin console can be compiled out.** `server.Options{UI: bool}` controls
  whether it is mounted, and a build tagged `noui` removes the embedded assets
  from the binary entirely, saving 33,312 bytes on this checkout's `cmd/llmux`
  build. Note the distinction: the flag governs what is *served*, the tag
  governs what is *linked*, and a program that imports only `core/gateway` links
  zero console bytes regardless of either.
- **An embeddability test suite** in `embedtest/`, a separate Go module, plus CI
  guards asserting that construction is inert, that `core/gateway` never imports
  the server or the console, that both build tags compile, and that a
  library-only host links no UI bytes.

### Changed

- `core/server` is now a thin adapter layer: request decoding, SSE framing,
  auth-decision plumbing, admin routes and the console mount. `ChatRaw` retains
  the raw-bytes passthrough so unknown upstream fields still survive the HTTP
  path.
- `Authorize` returns `(ctx, release func(), error)`. `release` is never nil and
  **must** be called; the HTTP middleware defers it. Failing to call it leaks a
  budget hold.
- Logging inside `core/` goes through the configured `*slog.Logger` instead of
  the package-level `log`.
- The egress guard now scans `core/gateway/` as well as `core/server/`. This is
  load-bearing, not cosmetic: with the old scope, dispatch moving into
  `core/gateway` would have escaped the check entirely.
- Test count: 558 → 562 top-level.

## [0.1.1] - 2026-08-09

### Added

- **Signed, checksummed releases — and a verifier that fails closed.** Nothing
  in the release pipeline previously vouched for the bytes it published. The
  release workflow now stages every asset into `release/`, emits a `SHA256SUMS`
  manifest **over that directory** (so "published" and "covered" are the same set
  by construction, not two hand-maintained lists), asserts one manifest line per
  staged asset, and attaches a sigstore build-provenance attestation minted from
  the workflow's OIDC identity — no long-lived signing key exists, so there is
  none to leak, own or rotate. A release that staged nothing, or whose manifest
  does not cover what it staged, is now a **red** release rather than a green one
  with an empty manifest. `scripts/verify.sh` is the user-facing half: it fetches
  the manifest, looks up the **exact** entry for the requested asset (string
  comparison on field 2 — a substring/regex match would let `…tar.gz.sig` answer
  for `…tar.gz`) and compares digests. Two outcomes only, verified or non-zero
  with a distinct diagnostic; there is no `--skip-verify` and **no path where an
  absent `SHA256SUMS` means "nothing to check"** — that shrug is the bug the file
  exists not to have, because it converts *"I don't know"* into *"it's fine"*.
  The release job runs `verify.sh` against its own output before publishing, so
  producer and consumer cannot drift apart silently.
- **`make verify-selftest`**, wired into `make gates` and into CI — 24
  synthetic-origin cases covering every refusal: manifest 404, manifest served as
  an HTML error page (both by content-type and by sniffing a lying one),
  empty/junk/truncated manifest, no entry for the asset, the `.sig` and
  regex-wildcard name traps (one arranged so a naive substring match would report
  **exit 0 on an artifact nobody vouched for**), asset 404, asset served as HTML,
  truncated download, digest mismatch, plaintext origin, missing curl or digest
  tool, and `--attest` with no `gh` installed. Each case asserts the exit code
  **and** that a diagnostic was printed — a guard that aborts silently reads as a
  crash, not a refusal, and "died at a pipeline under `set -e`" is precisely how
  a sibling installer's unreachable guard shipped.
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

- **Gated integration tests now skip loudly**: a run without Postgres/Redis
  prints which tests were skipped *and which properties went unverified*, the
  gated corpus size is checked against the source, and CI sets
  `LLMUX_REQUIRE_INTEGRATION=1` so a skip there is a failure rather than a green
  run with no cross-replica coverage.
- Documented what the sovereignty gate does **not** cover (price-catalog sync,
  control-plane seam, Redis, Postgres) in `docs/architecture.md`, plus a short
  copyable description of the pattern. The price-catalog sync is on by default
  and is now disclosed as such in the README and `docs/configuration.md`.

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

### Removed

- **The React/Vite admin SPA and the entire Node toolchain that built it.**
  The embedded dashboard at `/ui` is now `web/ui.html` — one hand-written
  467-line file, inline `<style>`/`<script>`, vanilla JS, no imports, no CDN.
  Same three tabs as before (usage/keys/models) as plain tables, tab state in
  the URL fragment, gateway URL + master key in `localStorage`, a 15s live
  poll — but no footer and no theme toggle (it follows
  `prefers-color-scheme` instead). Deleted: `web/dist/`, `web/src/`,
  `web/e2e/`, `web/public/`, `web/scripts/`, `web/index.html`,
  `package.json`, `package-lock.json`, and every vite/vitest/tailwind/
  postcss/playwright config file — there is no Node toolchain anywhere in
  this repo any more; `go build` alone is sufficient, including for UI
  changes. `web/THIRD-PARTY-NOTICES-GO.txt` (the Go binary's own dependency
  notices, unrelated to the browser page) is kept and now served at
  `/ui/licenses.txt`. New Go tests replace the retired JS suites:
  `web/embed_test.go` and `core/server/ui_test.go`.
- **`scripts/check-web-dist.sh` and the `web-dist-check` gate.** That gate
  existed to catch a committed `web/dist` bundle drifting from the
  `web/src` it was built from. With no build step and no committed bundle,
  that invariant no longer exists for anything to protect, so the script and
  the `web` / `web-dist-check` Makefile targets — and the Node/Playwright/
  dist-staleness steps in `.github/workflows/` — are gone too. `make gates`
  is now `fmt-check` + `verify-selftest`.

### Fixed

- **The brand mark never propagated past `brand/logo.svg`.** The "fill the mux
  body solid" decision only ever touched `brand/logo.svg` itself — every
  downstream copy (`web/public/favicon.svg`, `web/public/llmux.svg`,
  `web/src/components/Logo.jsx`'s in-app `Mark`, `assets/llmux-logo.svg` and
  its `site/assets/` twin, and `site/assets/llmux-mark.svg` — the actual
  favicon/nav-tile the marketing site and docs viewer serve) still drew the
  earlier white-outline wireframe glyph. Recoloured/re-derived all of them to
  the approved solid dark-ink (`#0A0F1A`) body on the teal tile, regenerated
  the PNG exports (`llmux-logo.png` ×2, `favicon-32.png`,
  `apple-touch-icon.png`, `llmux.png`) from the fixed SVGs, and rebuilt
  `web/dist` so the embedded binary ships the corrected mark too. Also removed
  `web/public/og.jpg`, an unreferenced pre-rebrand social-preview image (orange
  theme, old screenshot-style hero) that was still being embedded into the
  binary via `web/dist` despite nothing linking to it.
- Re-captured `docs/screenshots` and `site/screenshots` (landing, docs, and
  the three dashboard tabs) plus `web/public/shots/dashboard.jpg` against the
  corrected UI — this repo has no committed screenshot harness, so the capture
  reused `web/e2e/fixtures.js`'s `installApi` mock ad hoc. Incidentally
  current at capture time: the binary-size stat reads `~14MB`, not the
  previously-screenshotted `~9MB`, and the header CTA reads "Vulos OS", not
  "Vulos Cloud" — both are pre-existing copy that had already drifted from the
  stale screenshots, not something this change introduced.
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

[Unreleased]: https://github.com/vul-os/llmux/compare/v0.1.5...HEAD
[0.1.5]: https://github.com/vul-os/llmux/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/vul-os/llmux/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/vul-os/llmux/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/vul-os/llmux/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/vul-os/llmux/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/vul-os/llmux/releases/tag/v0.1.0

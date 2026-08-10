# Architecture

llmux is a Go **library first**: `core/gateway` is the whole dispatch path —
routing, retries, failover, sovereignty enforcement, BYOK, caching, pricing and
metering — with no HTTP surface of its own. `core/server` is one shell over
that library: the OpenAI-compatible HTTP API, streaming, auth and the embedded
admin console. It is not the only possible shell, and nothing in `core/gateway`
imports it. The **OpenAI HTTP schema is the canonical contract** either way:
providers are adapters behind it, routing and budget controls ride on standard
request fields plus `extra_headers` / `metadata`, and the streaming format is
byte-identical to OpenAI so every language's stream parser just works.

```text
core/                the open gateway (no cp dependency)
  openai/            canonical OpenAI wire types (the contract)
  gateway/           the library: New, Chat, ChatStream, Embed, Authorize, Run — no HTTP
  server/            HTTP shell over gateway/: routing to handlers, streaming, auth, metrics
  sovereign/         the sovereignty gate (where your AI runs; default-deny egress)
  provider/          Provider interface + SSE utilities
    passthrough/     OpenAI-shaped upstreams
    anthropic/ gemini/ cohere/ bedrock/ azure/   native adapters
  router/            routing + least-cost selection
  keys/              virtual keys, budgets, rate limits (Postgres + Redis)
  byok/              per-account provider keys, AES-256-GCM under an operator KEK
  cache/             exact + semantic response cache
  pricing/           catalog + live sync + cost accounting
  config/            JSON config loader
cmd/llmux/           the binary (server + CLI subcommands) — one shell among possible others
integration/cp/      OPTIONAL control-plane (billing/entitlements) adapter
web/                 admin console (ui.html, hand-written, no build step), embedded at /ui by core/server only
```

## llmux as a library

This section is the map. [Embedding llmux in Go](embedding.md) is the territory:
the whole `core/gateway` surface, the seams, the errors worth handling by type,
and what to do about each caveat below.

`core/gateway.New(cfg *config.Config, opts ...Option) (*Gateway, error)` builds
a `Gateway` and holds to five rules: it starts no goroutines (background work is
opt-in via `Run`); it reads no environment of its own (`config.Default()` /
`config.Load(path)` / `config.FromJSON(data)` are explicit calls the caller
makes, and those are what consult the environment); there is no package-level
mutable state or package logger, so two gateways in one process never interfere;
readiness is explicit, never implied; and it is pure core with opt-in I/O.
Dispatch with `Chat` / `ChatStream` / `Embed`; `Run(ctx)` (or the non-blocking
`Start(ctx)`) opts into the price-catalog syncer, the file key store's spend
flusher, and a Redis ping — none of which run otherwise.

Two things happen regardless, and are documented here rather than left to be
discovered:

- **If `cfg.Postgres` is set, `New` connects and migrates eagerly.** Building
  the Postgres key store (`keys.NewPGStore`) is the one qualification to "New
  opens no sockets" — an explicitly opted-into remote dependency. With no DSN
  (every default and library configuration), `New` opens nothing.
- **`New` reads `os.Getenv` for any provider configured with `api_key_env`.**
  Each provider adapter resolves its credential at construction time
  (`config.ProviderConfig.ResolveKey`), so a provider entry naming
  `"api_key_env": "OPENAI_API_KEY"` is read from the environment the moment
  `New` builds that adapter. This is config-directed, not auto-detected: it
  only reads the specific env var names present in the config you pass (or
  that `config.Default()`'s own auto-detection already wrote there) — never an
  arbitrary variable `New` decided to go looking for.

`Authorize(ctx, token) (context.Context, func(), error)` is the single auth
path: `core/server`'s HTTP middleware and an in-process host both call it, so an
embedder cannot get a laxer check than a network client. The returned `release`
function is **never nil** and **must always be called**, including on error —
it frees a budget-gate reservation the call may have placed; skipping it is an
easy leak. When no credential wall is configured at all (no static keys, no
external identity), `Authorize` is a no-op that returns `ctx` unchanged — the
standalone local-sidecar posture, where an in-process host is already trusted.

`core/server` is optional in two independent, and easily confused, ways:

- **`server.Options{UI: bool}`** (default `true`) decides whether a *running*
  server *mounts* the console route at `/ui`. It is a runtime choice made by
  whatever Go program constructs the `*server.Server` — the stock `cmd/llmux`
  binary always passes the default (`UI: true`) and has no flag for it. Setting
  it `false` 404s every `/ui*` path, but the console's bytes are still linked
  into that binary: `core/server` imports `web/` unconditionally, so `go:embed`
  still embeds `ui.html`.
- **The `noui` build tag** decides whether the console is *compiled in* at all.
  With it, `web.Enabled()` reports false, `HTML()`/`Licenses()` return `nil`,
  and `/ui*` serves a small JSON stub saying the console was not built in
  rather than 404ing (which would read as "wrong URL"). This is the tag that
  actually removes the bytes; `Options.UI` alone does not.
- Importing `core/gateway` alone (not `core/server`) never links `web/` at
  all — only `core/server` imports it — so a pure library host has zero UI
  bytes in it regardless of build tags. Measured in this checkout
  (darwin/arm64, go1.25.12): a program importing only `core/gateway` builds to
  **15,293,346 bytes**; adding `core/server` with the console (the default)
  brings it to **17,114,706 bytes** (+1,821,360 bytes for the HTTP shell and
  the embedded console together). See
  [Operations → building without the console](operations.md#building-without-the-console-noui)
  for the `noui`-specific delta.

### The same library, from a non-Go host

`ffi/` builds `core/gateway` as a C shared library — seven functions, JSON in and
JSON out, the same JSON the HTTP API uses. It is a **separate Go module**
(`github.com/vul-os/llmux-ffi`) deliberately outside this repo's import prefix,
so Go's `internal/` rule applies to it exactly as it does to any third-party
embedder: the C ABI is *evidence* that the exported API is sufficient, not a
privileged insider. Keeping it out of the main module also keeps cgo out of
`go build ./...` and out of the `-tags noui` build.

The costs of putting the Go runtime inside someone else's process — signal
handlers, no fork-safety, a ~12–17 MB artifact, and a platform matrix with two
targets that do not exist — are in [The C ABI](c-abi.md#the-costs). For several
hosts the sidecar is the better answer, and
[Choosing a mode](choosing-a-mode.md) is the page that decides it.

## The sovereignty gate (where your AI runs)

llmux is Vulos's **sovereign** LLM gateway: inference runs on **your** box by
default, and a request is **never silently sent to a company that mines you**.
This is enforced, not documented-hope. `core/sovereign` classifies every
configured provider by *where its traffic goes*, and the gate is called
**before any network call on every dispatch path** — chat, streaming chat,
embeddings, the semantic-cache embedder, and all model-bearing modality routes
(`/v1/completions`, `/v1/responses`, images, audio
speech/transcriptions/translations, moderations, rerank). Chat, streaming chat,
embeddings and the semantic-cache embedder call it from inside `core/gateway`
(`core/gateway/sovereignty.go`, `enforceSovereignty`) — the library path, so an
embedding host gets the same check a network client does. The forwarded
modality routes are HTTP-only and still live in `core/server`
(`forward.go`/`transcription.go`), which calls the identical check through a
one-line delegate (`core/server/sovereignty.go`'s `enforceSovereignty` calls
`Gateway.EnforceSovereignty`) — one gate, reached from two call sites.

Providers resolve to a 4-tier dial, most→least private:

| Tier | What it is | Default |
|---|---|---|
| **local** | inference on THIS box (loopback / unix socket) | always allowed |
| **sovereign** | an operator-declared endpoint the operator vouches for (unverified by Vulos) | allowed on the operator's declaration |
| **brokered** | a named third party under a claimed no-train agreement | blocked until `allow_brokered` |
| **external** | any other off-box endpoint (may mine/train) | **blocked** until `allow_egress` |

The gate **fails closed**: an empty/unparseable base URL, an off-box endpoint
marked `local`, or any unrecognized tier is treated as **external and blocked**.
Nothing silently upgrades — `sovereign`/`brokered` are explicit operator config
declarations; an unmarked off-box endpoint derives `external` from its locality.
A blocked provider never opens a socket; the denial is logged and counted
(`egress_blocked` metric), and every *permitted* off-box call is logged with its
tier so egress is always observable, never silent. On failover, a blocked
primary is skipped so a local fallback can still serve; if every target is
blocked the 403 surfaces. `/health` itself is unauthenticated for every caller
(a minimal `{"status":"ok"}`), but the full posture — each provider's tier,
label, and whether it may egress — is disclosed only to the master key (or, on
a keyless gateway, only to a loopback caller); see [API reference](api.md).

Operators opt a provider in per-provider in the config (never globally):
`"tier": "sovereign"`, `"allow_brokered": true`, or `"allow_egress": true`.

### What the gate does not cover

The gate governs **inference dispatch** — the paths a prompt or a completion can
travel. It is not a process-wide firewall, and the following outbound
connections do not pass through it. None of them carries prompt or completion
text:

| Path | When it dials | What it sends |
|---|---|---|
| **Price-catalog sync** (`core/pricing`) | **On by default** — `config.Default()` ships two public feeds (openrouter.ai, raw.githubusercontent.com); a GET at startup and every `sync_interval_minutes` | Nothing. It is a plain GET of a public price list — no prompt, no key, no usage. Disable with `"pricing": {"sources": []}`; the built-in seed catalog still prices requests offline, or point `sources` at your own mirror. |
| **Control-plane seam** (`integration/cp`) | Only when `LLMUX_CP_URL` is set | Billing counts (model, tokens, account) — never prompts or completions |
| **Redis** (cache + rate limits) — `core/gateway/gateway.go` | Client constructed whenever `redis` is configured; `redis.NewClient` itself does not dial, but `Gateway.Start` pings it, so a misconfigured address fails at startup, not on the first request | Cache **values**, which can contain completions — treat Redis as inside your sovereignty boundary |
| **Postgres** (key spend/budgets) — `core/keys` (`NewPGStore`, called from `core/gateway/gateway.go`) | Connects and migrates **eagerly, inside `New`** whenever `cfg.Postgres` is set — the one exception to "`New` opens no sockets" (see [llmux as a library](#llmux-as-a-library)) | Key names, spend, budgets |

Everything in that table is enumerated in the `outboundDialSites` registry in
`core/sovereign/egress_guard_test.go`. Adding an outbound connection anywhere in
the repo fails CI until it is listed there with a reason — so this table cannot
quietly fall out of date.

### The pattern (copyable)

The reason this holds is not the tier table; it is the shape. Four rules, and
they transfer to any service that must not phone home by accident:

1. **One classifier, built once from config, consulted per call.** A pure
   function maps each configured destination to a verdict (`core/sovereign`:
   base URL + operator marking → tier → allowed). It imports only the config
   package, so it is trivially testable and cannot itself dial anything.
2. **Fail closed on the unknown.** Empty base URL, unparseable URL, unknown
   provider name, unrecognized tier, off-box endpoint dishonestly marked
   `local` — all resolve to *blocked external*, never to *allowed*. A
   classification bug then costs availability, not privacy.
3. **The check sits in the same function as the dial, immediately before it.**
   Not in a constructor, not in middleware two frames up. `enforceSovereignty`
   is called in each of the six dispatch functions, and the guard test enforces
   *same function* precisely because "a caller up the chain checks it" is the
   reasoning that produces bypasses.
4. **Make it structural, not disciplinary.** Behavioral tests prove today's
   paths are gated; they say nothing about tomorrow's seventh path. So a test
   parses the source: every call to an egress-capable provider method must sit
   in a function that also calls the gate, and every file in the repo that can
   open a socket must be registered with a written reason. Both assert coverage
   floors, so a broken scan fails instead of passing vacuously. Adding an
   ungated egress path is then a CI failure on the day it is written, naming the
   file and line.

The escape hatch matters too: the gate is a gate, not a wall. Every block has a
matching per-provider opt-in, each denial is logged and counted, and each
*permitted* off-box call is logged with its tier — so egress is always
observable, never silent, in both directions.

## The canonical contract

Every provider implements one `Provider` interface and speaks only the canonical
OpenAI types. Provider-specific quirks — Anthropic's content blocks, Gemini's
schema, Bedrock's signing — stay behind that seam. This is what lets any OpenAI
SDK work unchanged regardless of which provider ultimately serves the request.

## The control-plane is isolated

The `core` packages **never import** `integration/cp`. The optional
control-plane adapter is wired only by `cmd/llmux`, and only when `LLMUX_CP_URL`
is set. Delete `integration/cp` entirely and the standalone build still compiles
and runs. See the [control-plane seam](control-plane.md) for details.

## Where llmux sits in Vulos

llmux is the **LLM access layer** for the Vulos suite. In a Vulos deployment the
**box is the authority** (it holds your data and runs your sovereign services),
**relay** is the single reachability ingress, and **cloud** is a content-blind
control plane (billing/entitlements only). llmux runs as one of the box's
sovereign services: its default-local sovereignty gate is exactly the box-as-
authority principle applied to inference. The optional `integration/cp` adapter
is how the box reports metered usage to the cloud control plane — it never sends
prompts or completions there, only billing counts.

Standalone (no cp, no Vulos suite) llmux is a complete self-hosted gateway on
its own; the same binary and code path serve both self-host and managed.

## Related

- [Embedding llmux in Go](embedding.md) — the full `core/gateway` API, the seams, and the errors worth handling by type
- [Choosing a mode](choosing-a-mode.md) — server vs sidecar vs library vs C ABI
- [The C ABI](c-abi.md) — the same library from a non-Go host
- [Client examples → embed it locally](client-examples.md#embed-it-locally-no-separate-server-to-run) — copy-paste `gateway.New` / `llmux.New` usage
- [Operations → building without the console](operations.md#building-without-the-console-noui) — the `noui` tag, `Options.UI`, measured sizes
- [Connecting providers](GETTING-STARTED.md#2-connect-providers) — native adapters vs. passthrough, and adapter stability
- [Model routing and selection](ADMIN-GUIDE.md#model-routing-and-selection) — how a model name resolves to a provider
- [Control-plane seam](control-plane.md) — the optional cloud billing adapter
- [LLM access: BYOK vs central](LLM-ACCESS.md) — per-account key resolution and metering

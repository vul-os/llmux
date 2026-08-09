# Embedding llmux in Go

`core/gateway` is llmux as a library. It is the whole dispatch path — routing,
retries, failover, the sovereignty gate, BYOK, caching, pricing and metering —
with no HTTP surface of its own. `core/server` is one shell over it, and nothing
in `core/gateway` imports that shell.

Embedding is not a stripped-down mode. It is the same code the server runs,
called without the socket in the middle.

```go
import "github.com/vul-os/llmux/core/gateway"
```

There is no separate library artifact to install and no build tag to set. If you
want the shortest possible version of this page, read
[the five rules](#the-five-rules-and-the-three-exceptions) and
[authorization](#authorization-is-one-call-and-you-must-make-it), then come
back.

## Hello, gateway

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
)

func main() {
	cfg := config.Default() // auto-detects providers from the environment
	gw, err := gateway.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer gw.Close()

	res, err := gw.Chat(context.Background(), &openai.ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Response.Choices[0].Message.Content.Text)
	fmt.Println("served by", res.Provider, "cache hit:", res.CacheHit, "byok:", res.BYOK)
}
```

No listener, no port, no `/health` to poll, no child process to reap. The
provider call is the only socket this program opens.

`sdks/go/llmux.New(llmux.Options{})` is a three-line wrapper over exactly this;
use whichever reads better in your codebase. The wrapper's `Options.Addr` and
`Options.ReadyTimeout` are ignored — they only mean something for the deprecated
loopback shim, which is **not** the sidecar.

**Not writing Go?** This same gateway, in this same in-process shape, is
reachable from fourteen other languages: thirteen load it through the
[C ABI](c-abi.md)'s six functions, and all fifteen packages can alternatively
spawn and supervise the binary as a sidecar. You are unlikely to need to write a
binding — see [Language packages](sdks.md#a-first-call-in-every-language) for a
first call in each. Note that the costs on this page are the Go-native ones;
the C-ABI hosts pay several more, including fork-safety and a platform matrix
with real gaps.

## Building a config

Three constructors, all in `core/config`:

| Call | Source | Reads the environment |
|---|---|---|
| `config.Default()` | Built-in defaults, providers auto-detected | **Yes** — detection plus `LLMUX_*` overrides |
| `config.Load(path)` | Defaults, then the JSON file at `path` (a missing file is not an error), then env | **Yes** |
| `config.FromJSON(data)` | Defaults, then the JSON document, then env — `Load` without a file | **Yes** |

All three apply defaults and run `Validate` before returning. Do not
`json.Unmarshal` into a zero `config.Config` yourself: you would silently drop
every default (no retries, no cache bound, no auto-detected local backend) and
skip validation entirely.

If you want a configuration that reads *nothing* from the environment, build the
`config.Config` struct literally in Go and hand that to `gateway.New`. That is
the only path with no environment read at all — and note that it still resolves
`api_key_env` entries you put in it yourself, because that is what those entries
mean.

## The five rules, and the three exceptions

`gateway.New(cfg *config.Config, opts ...Option) (*Gateway, error)` holds to
five rules:

1. `New` starts **no goroutines**. Background work is opt-in via `Run`.
2. `New` reads **no environment of its own**.
3. **No package-level mutable state and no package logger.** Everything hangs
   off the `*Gateway`, so two gateways in one process never interfere.
4. Readiness is **explicit** (`Start` / `Run`), never implied.
5. Pure core, opt-in I/O.

Three things happen anyway. They are documented here rather than left to be
discovered:

- **If `cfg.Postgres` is set, `New` connects and migrates eagerly.** Building
  the Postgres key store (`keys.NewPGStore`) is the one qualification to "New
  opens no sockets" — an explicitly opted-into remote dependency. With no DSN
  configured, which is every default and every library configuration, `New`
  opens nothing. A Redis address is *not* an exception: `redis.NewClient` does
  not dial, the pool connects on first use, and `Start` is what pings it.
- **`New` reads `os.Getenv` for any provider configured with `api_key_env`.**
  Each provider adapter resolves its credential at construction
  (`config.ProviderConfig.ResolveKey`), so an entry naming
  `"api_key_env": "OPENAI_API_KEY"` is read from the environment the moment
  `New` builds that adapter. This is config-directed, not auto-detected: it
  reads only the specific variable names present in the config you passed — or
  that `config.Default()`'s auto-detection already wrote there — and never a
  variable `New` decided to go looking for. Rule 2 is about `New` not
  *consulting* the environment; it is not a promise that a config which says
  "read this variable" will be ignored.
- **`New` reads two local files, if you configured them:** the on-disk price
  cache at `cfg.Pricing.CatalogPath` (a warm start, failure ignored) and the
  price-override file at `cfg.Pricing.OverridePath` (applied synchronously so
  overrides are in force before the first request). Both are local reads, both
  are off by default.

### Options

Options never start anything.

| Option | Wires |
|---|---|
| `WithLogger(*slog.Logger)` | The gateway's structured logger. `nil` is ignored |
| `WithIdentity(Identity)` | An external identity resolver — also *activates* the authenticated path even with no static keys configured |
| `WithBudgetGate(BudgetGate)` | An external budget/entitlement gate |
| `WithBYOKStore(BYOKStore)` | A per-account provider-credential store |
| `WithUsageLogger(UsageLogger)` | A usage sink — `NewJSONLUsageLogger(w)` writes JSONL to any `io.Writer` |

Each has a matching `Set…` method for wiring after construction.

## Dispatch

| Call | Returns | Notes |
|---|---|---|
| `Chat(ctx, req)` | `(*Result, error)` | Marshals `req` itself |
| `ChatRaw(ctx, req, raw)` | `(*Result, error)` | Sends the caller's original bytes, so fields llmux does not model survive the hop to a passthrough provider |
| `ChatStream(ctx, req, yield)` | `error` | `yield` is `func(*openai.ChatCompletionChunk) error`, called once per chunk |
| `Embed(ctx, req)` | `(*openai.EmbeddingResponse, error)` | Same gate, routing, metering |
| `Models()` / `ModelList()` | `[]string` / `openai.ModelList` | What `GET /v1/models` serves, from memory |
| `Prepare(ctx, model)` | `(router.Resolution, error)` | Resolve a route without dispatching |

`Result` is the thing an in-process caller gets that an HTTP client cannot,
because the HTTP shell flattens it into a response body:

```go
type Result struct {
	Response *openai.ChatCompletionResponse // the canonical response, usage.cost attached
	Provider string                         // who ACTUALLY served, after failover
	BYOK     bool                           // served on the account's own key (unmetered)
	CacheHit bool                           // served from the exact/semantic cache
	Headers  http.Header                    // allow-listed upstream headers; nil on a cache hit
	Body     []byte                         // serialized exactly once — write these bytes verbatim
}
```

Streaming:

```go
err := gw.ChatStream(ctx, req, func(c *openai.ChatCompletionChunk) error {
	if len(c.Choices) > 0 {
		fmt.Print(c.Choices[0].Delta.Content)
	}
	return nil // returning an error stops the stream and is returned to you
})
```

Streaming is **always metered**: llmux injects `stream_options.include_usage`
upstream so a final usage chunk exists, and meters whatever was served even when
the stream breaks mid-flight. Cancelling `ctx` cancels the in-flight upstream
request.

## Authorization is one call, and you must make it

There is no middleware in library mode, because there is no HTTP. The check is
one function — and it is the *same function* `core/server`'s middleware calls,
so an embedder cannot accidentally get a laxer check than a network client:

```go
ctx, release, err := gw.Authorize(ctx, token)
defer release()
if err != nil {
	// gateway.ErrUnauthorized, or *gateway.AuthError with .RateLimited / .Denied
	return err
}
res, err := gw.Chat(ctx, req)
```

`Authorize` returns `(context.Context, func(), error)`. Three things about that
signature are load-bearing:

- **`release` is never nil, and must always be called** — including on the error
  paths, including when you return early. It frees the in-flight reservation the
  budget gate placed. Skip it and the reservation leaks: the key's outstanding
  in-flight total only ever rises, and once it reaches the budget every
  subsequent request on that key is denied with "budget exceeded" while the
  recorded spend says otherwise. `defer release()` immediately after the call,
  before you inspect `err`, is the shape that cannot get this wrong.
- **The returned `ctx` is the one you must pass on.** It carries the resolved
  key and account id, which is what the dispatch paths read for per-key model
  allow-lists, cache scoping, BYOK resolution and spend recording. Authorizing
  and then calling `gw.Chat` with the *original* context compiles, runs, and
  quietly enforces none of that.
- **When no credential wall is configured at all** — no static keys, no external
  identity — `Authorize` is a no-op that returns `ctx` unchanged. That is the
  standalone posture, where an in-process host is already trusted. It also means
  a test that never configures keys will not tell you whether your `release`
  handling is right.

`gw.IdentityActive()` reports whether the authenticated path is in force.

### Errors worth handling by type

| Error | Means | The HTTP shell maps it to |
|---|---|---|
| `ErrUnauthorized` | Unknown bearer token | 401 |
| `*AuthError{RateLimited: true}` | Over the per-minute cap | 429 |
| `*AuthError{Denied: true}` | Over budget, suspended, or disabled | 402 |
| `ErrNoModel` | The request omitted `model` | 400 |
| `ErrModelNotAllowed` | The key's allow-list excludes this model | 403 |
| `*UnmeterableError` | A budgeted key asked for a model the catalog cannot price — refused **pre-flight**, so no upstream call happened | 400 |
| `gateway.AsProviderError(err)` | Non-nil for an upstream failure; `.Status()` is the upstream's code | passthrough |

`UnmeterableError` is a fail-closed refusal, not a bug: serving it would record
$0 spend, the key's budget would never trip, and a budgeted key could burn
unbounded real spend on the operator's central keys.

## Background work is opt-in, and it is not free

`New` starts nothing. `Start(ctx)` opts in and returns immediately; `Run(ctx)`
opts in and blocks until `ctx` is done. Both start exactly this:

- A **Redis ping**, so a misconfigured address fails at startup rather than on
  the first request.
- The **price-catalog syncer**, if any pricing source is configured.
- The **file key store's spend flusher**, if `cfg.KeyStorePath` is set.

The syncer matters for a sovereign host: `config.Default()` ships two remote
pricing sources (`openrouter.ai` and a raw GitHub URL), so **calling `Run` on a
default config makes periodic outbound requests to those two hosts** — not for
inference, but they are outbound requests all the same. A library caller that
never calls `Start` or `Run` gets zero background traffic and zero outbound
GETs; the built-in catalog and any on-disk cache still price requests. To keep
the background sync but drop the network, set `cfg.Pricing.Sources` to nil and
use `cfg.Pricing.Overrides` or `OverridePath`.

`Close()` releases the Redis client and the Postgres pool. It does not stop
background work — cancelling `Run`'s context does that — and it is safe on a
gateway that was never started.

## The sovereignty gate still applies

Embedding does not opt you out of it. `core/sovereign` classifies every
configured provider by where its traffic goes, and the gate runs **before any
network call on every dispatch path** — unary chat, streaming chat and
embeddings alike. A blocked target is skipped so a local fallback can still
serve; if every target is blocked, the refusal surfaces as the error. In library
mode there is no HTTP status to hide behind, so you get the error directly.

`gw.Sovereign()` returns the policy and `gw.Metrics().EgressBlocked()` counts
refusals, if you want to surface either in your own health surface. See
[Architecture → the sovereignty gate](architecture.md#the-sovereignty-gate-where-your-ai-runs).

## Observability without a metrics endpoint

- `gw.Metrics()` — in-process counters (`Inflight`, `UpstreamErrors`,
  `CacheHits`, `EgressBlocked`), and `WriteProm(w)` writes the Prometheus
  exposition text to any `io.Writer`, so you can mount it on your own router
  without importing `core/server`.
- `gw.Stats().Snapshot()` — the aggregate the admin console renders, as a plain
  `map[string]any`.
- `WithUsageLogger` — one `UsageRecord` per request, including model, provider,
  tokens, cost, cache hit and BYOK.

## Two gateways in one process

Supported, and tested. Provider adapter options (response cap, `drop_params`)
are per-gateway rather than package-level — they used to be package variables
that `server.New` mutated, which meant two gateways in one process silently
corrupted each other's settings. If you need one gateway with an egress-allowed
provider set and another that is local-only, that composes.

## Testing your embed

Point a provider at an `httptest.Server` and there is nothing else to stub:

```go
cfg := &config.Config{Providers: []config.ProviderConfig{{
	Name: "fake", Type: "openai", BaseURL: srv.URL, APIKey: "test",
}}}
gw, err := gateway.New(cfg)
```

No listener, no port allocation, no readiness poll, no child process — a
gateway test is an ordinary unit test. The repo's own `embedtest/` module is
the harder version of this: it is a **separate Go module** that imports llmux
from outside its own import prefix, which is how the embeddability guards prove
the exported API is sufficient for a third-party host rather than quietly
relying on `internal/` access. `ffi/` does the same thing for the C ABI.

## What you do not get

- **No HTTP surface.** No `/v1/chat/completions`, no SSE framing, no
  `/metrics` endpoint, no `/health`. If you want those, embed `core/server`
  too — it is a shell over the same gateway, not an alternative to it.
- **No admin console.** Importing `core/gateway` never links `web/`; only
  `core/server` imports it. A library host has **zero console bytes** in it
  regardless of build tags. Measured in this checkout (darwin/arm64,
  go1.25.12): a program importing only `core/gateway` builds to
  **15,293,346 bytes**; adding `core/server` with the console brings it to
  **17,114,706 bytes**. The `noui` tag's own delta — **33,312 bytes** on
  `cmd/llmux` — only exists for hosts that *do* import the shell. See
  [Operations → building without the console](operations.md#building-without-the-console-noui).
- **No authentication you did not call.** See
  [above](#authorization-is-one-call-and-you-must-make-it).
- **No background work you did not start.** See
  [above](#background-work-is-opt-in-and-it-is-not-free).

## Related

- [Choosing a mode](choosing-a-mode.md) — library vs sidecar vs server vs C ABI
- [The C ABI](c-abi.md) — the same in-process gateway, from a non-Go host
- [Language packages](sdks.md) — the fifteen, with a first call in each
- [Architecture](architecture.md) — how the packages are laid out
- [Configuration](configuration.md) — every config field and environment override
- [Client examples](client-examples.md) — the HTTP path, in 17+ languages

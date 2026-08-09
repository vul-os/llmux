# llmux for Go

**Go is the special case, and it is worth being blunt about it: in Go there is
no FFI, no shared library, and no platform matrix. You import a package.**

```go
gw, err := llmux.New(llmux.Options{})   // no listener, no port, no HTTP hop
defer gw.Close()
res, err := gw.Chat(ctx, &openai.ChatCompletionRequest{ /* ... */ })
```

Every other language in `sdks/` reaches this same `*gateway.Gateway` by loading
a 12–17 MB C shared library that puts the Go runtime inside their process, or by
supervising a second process and talking HTTP to it. Go pays for neither. When
you read the Rust or Swift README next to this one, that gap is the thing being
described.

## The two examples

| | what it does | when you want it |
| --- | --- | --- |
| [`examples/direct`](examples/direct) | imports the gateway, calls it in-process | **the default.** No port, no second process, no serialization |
| [`examples/sidecar`](examples/sidecar) | spawns and supervises `llmux serve`, talks HTTP | per-tenant keys and budgets; one gateway shared by several processes; crash isolation |

Run both offline, with no provider keys and no network, against the
OpenAI-compatible fake in `ffi/fakeupstream`:

```
./sdks/go/examples/run.sh            # both
./sdks/go/examples/run.sh direct
./sdks/go/examples/run.sh sidecar
```

Real output from `run.sh` on darwin/arm64, Go 1.25.12, llmux 0.1.2:

```
==> direct (in-process, no port)
models: google/gemini-1.5-flash, openai/gpt-4o, openai/gpt-4o-mini, anthropic/claude-3-5-haiku, ...
chat:   one two three four
served: provider=fake cache_hit=false byok=false tokens=7+5
stream: one two three four
chunks: 6

==> sidecar (child process over HTTP)
sidecar: http://127.0.0.1:59497 pid 88240
models: anthropic/claude-3-5-haiku, deepseek/deepseek-chat, ...
chat:   one two three four
tokens: 7+5
stream: one two three four
chunks: 6
```

Against your own providers, drop the runner and pass a model:

```
export OPENAI_API_KEY=sk-...
go run ./sdks/go/examples/direct -model gpt-4o-mini -prompt 'say hi'
```

## What in-process actually buys you

Look at the two `chat:` lines above. They are identical. Now look at the line
under them — `served: provider=fake cache_hit=false byok=false` — which only the
direct example can print.

`gw.Chat` returns a `*gateway.Result`, not just an OpenAI response:

```go
type Result struct {
    Response *openai.ChatCompletionResponse
    Provider string       // who ACTUALLY served, after failover
    BYOK     bool         // served on the account's own key, therefore unmetered
    CacheHit bool         // no provider was called at all
    Headers  http.Header  // the upstream's rate-limit / retry-after headers
    Body     []byte       // serialized exactly once
}
```

The HTTP shell flattens all of that into a response body and throws the rest
away. That, plus not running a second process and not binding a port, is the
case for embedding.

**It is not latency.** `ffi/README.md` measures the boundary at ~4 µs
in-process against ~46 µs over loopback, and a whole `chat` call at ~80–92 µs
against ~102–109 µs. A real model answers in hundreds of milliseconds. Anyone
choosing in-process for the microseconds is optimising the wrong thing.

## The package

`sdks/go/llmux` is a thin convenience, not a layer you are required to use.
`llmux.New(Options{Config: cfg})` is `gateway.New(cfg)` with `config.Default()`
substituted for a nil config. Importing `core/gateway` and `core/config`
directly is equally supported and costs you nothing.

### `Start` is a loopback shim, and is deprecated

```go
// Deprecated: this is a loopback shim, not embedding.
local, err := llmux.Start(llmux.Options{})
defer local.Close()
// hand local.OpenAIBaseURL() to any OpenAI-compatible HTTP client
```

`Start` runs the full HTTP server on an ephemeral `127.0.0.1` port **inside your
own process** and hands you a base URL. It costs a port, a listener, and a JSON
round trip on every call, and it gives you none of the `Result` fields above —
you pay the HTTP tax without getting process isolation in return.

It is kept, and will stay kept, for exactly one job: handing a base URL to an
OpenAI-compatible HTTP client you did not write and cannot change. If that is
your situation it is the right tool. If it is not, use `New`.

Note that `Start` is **not** the sidecar. The sidecar is a separate `llmux
serve` process — that is what [`examples/sidecar`](examples/sidecar) shows, and
it is the one that gets you auth, budgets, sharing and crash isolation.

## Honesty notes

These apply to every language's SDK. For Go, most of them are moot, and saying
which and why is the point.

1. **The Go runtime in the host process.** Moot. It is already your runtime.
2. **Not fork-safe.** Moot for the direct path — there is no C shared library
   to be left in a broken state by `fork()` without `exec()`. (Go programs do
   not meaningfully `fork()` without `exec()` in the first place.)
3. **A 12–17 MB shared library.** Moot. There is no shared library. You link
   llmux into your binary and the linker drops what you do not call.
4. **Prebuilt binaries only for darwin/arm64 and linux/arm64.** Moot, and this
   is the biggest difference. The C ABI ships prebuilt artifacts for two
   platforms; linux/amd64 is CI-only and **windows/amd64 and darwin/amd64 do
   not exist at all**. The Go path has no platform matrix: `go get` builds from
   source for whatever `GOOS`/`GOARCH` you are on, including the three the
   shared library does not cover.
5. **Latency is not the reason to embed.** True here too. See above.
6. **Is the sidecar the better default for Go?** No. Direct is the default in
   Go and the sidecar is the considered exception — the reverse of the advice
   several other languages get. Pick the sidecar when you need per-tenant auth
   and budgets, a gateway shared across processes, or crash isolation.

## Layout

```
sdks/go/
  llmux/            the convenience package (New, and the deprecated Start)
  examples/
    direct/         in-process gateway: models, chat, streaming chat
    sidecar/        spawn `llmux serve`, then HTTP + SSE against it
    run.sh          runs both offline against ffi/fakeupstream
  README.md
```

`sdks/go` has no `go.mod` of its own — it is part of the root
`github.com/vul-os/llmux` module, so `go test ./...` at the repo root covers it.

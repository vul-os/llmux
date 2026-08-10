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

Run all three offline, with no provider keys and no network — direct and
sidecar against the OpenAI-compatible fake in `ffi/fakeupstream`, cancel
against `sdks/fake-upstream.py` (see [below](#cancelling-a-stream-context-not-a-symbol)
for why that one needs a different fake):

```
./sdks/go/examples/run.sh            # all three
./sdks/go/examples/run.sh direct
./sdks/go/examples/run.sh sidecar
./sdks/go/examples/run.sh cancel
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

==> starting sdks/fake-upstream.py (100ms/chunk, so there is something to cancel)
    http://127.0.0.1:55658
==> direct, context cancellation (cancel after 3 delivered chunks)
stream: one two three
consumer chunks: 3
stream error: context canceled
upstream counter: http://127.0.0.1:55658/generated
handle after cancel: models -> openai/gpt-4o-mini, anthropic/claude-3-5-sonnet, ...
upstream generated: {"generated": 3, "streams": 1, "disconnects": 0}
```

The `chunks: 6` lines above are an **uninterrupted** four-word stream (four
content chunks, a finish-reason chunk, a usage chunk) — nothing in the
direct/sidecar walkthrough cancels anything. The cancellation numbers are
covered on their own terms below.

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

## Cancelling a stream: context, not a symbol

llmux v0.1.5 added `llmux_cancel` as the seventh symbol in the C ABI
(`ffi/include/llmux.h`): a way to abort a blocked `llmux_stream` or
`llmux_call` — from another thread, or safely from inside the chunk callback —
without tearing down the whole gateway the way `llmux_close` does. Every other
language in `sdks/` binds that symbol to get this. Go does not need to,
because Go never had the gap `llmux_cancel` closes.

`gw.Chat`, `gw.ChatStream`, and the HTTP handlers behind `Start`'s loopback
server all take, or thread through, an ordinary `context.Context`, and
`core/gateway` passes it unmodified all the way down to the `http.Request` the
provider makes: `core/provider/passthrough.go` calls
`http.NewRequestWithContext(ctx, ...)` and reads the streamed body through
that same request. Cancel the context and `net/http` closes the connection out
from under the read loop. That is the entire mechanism `llmux_cancel`
implements in C — the Go standard library already does it, for every
`ctx.Done()` in your program, whether or not llmux exists.

```go
ctx, cancel := context.WithCancel(context.Background())
var chunks int
err := gw.ChatStream(ctx, req, func(c *openai.ChatCompletionChunk) error {
    chunks++
    if chunks == 3 {
        cancel() // safe from here, and from another goroutine — see below
    }
    return nil
})
// err wraps context.Canceled. Tokens already served are still metered.
```

This holds for both paths this package offers, and a Go user should not have
to know which one they are on to get it:

- **`New` / `gw.ChatStream`** (direct, in-process): proved by
  `sdks/go/llmux/cancel_test.go`'s `TestChatStreamCancelStopsUpstream`,
  against an in-process counting fake upstream.
- **`Start` / `Local`** (the loopback HTTP shim): proved by the same file's
  `TestStartStreamCancelStopsUpstream`. The caller cancels the context on
  *their own* `http.NewRequestWithContext` call against
  `local.OpenAIBaseURL()`; `net/http`'s server closes `r.Context()` when that
  connection drops, and `core/server/chat.go`'s `streamChat` hands
  `r.Context()` to `Gateway.ChatStreamSink` unchanged — the identical
  propagation the direct path proves, one HTTP hop further out.

Calling `cancel()` **from inside the chunk callback is safe** — it does not
deadlock, unlike calling `gw.Close()` from inside a callback, which blocks
waiting for the very call that is running the callback (see `llmux_close`'s
doc comment in `ffi/include/llmux.h`; the same hazard applies to
`(*gateway.Gateway).Close` here, and for the same reason). The callback is
often the only place a single-threaded consumer — an early `break` out of an
iterator, a `select` loop that has decided it is done — can reach, so this has
to work, not merely be possible from a second goroutine.

### The measured numbers

Against `sdks/fake-upstream.py --chunk-delay-ms 100 --text "one two three four
five six seven eight nine ten"` — the same harness the other languages'
READMEs cite — cancelling after 3 delivered chunks:

```
stream: one two three
consumer chunks: 3
stream error: context canceled
upstream counter: http://127.0.0.1:55658/generated
handle after cancel: models -> openai/gpt-4o-mini, anthropic/claude-3-5-sonnet, ...
upstream generated: {"generated": 3, "streams": 1, "disconnects": 0}
```

Consumer stopped at 3; the upstream generated **3 of 12**. Not 10 (the last
word was never sent) and not 12 (ten content chunks plus the forced
finish-reason chunk and usage chunk an uncancelled run always produces).

The demo proves the gateway survived with a `models` call rather than a second
stream, and the reason is the counter: `/generated` is cumulative for the life
of the harness, so a second, uncancelled stream would add its twelve chunks and
report 15 — a measurement spoiled to make a second point. That second point is
made by `TestChatStreamCancelIsPerCall`, which owns its own counter.

This is `./sdks/go/examples/run.sh cancel`, unedited, and it is also
what `-cancel-demo` in `examples/direct` runs; the C ABI's own probe, against
the same text and the same delay, measured the identical 3-vs-12 split. Go
reaches the same number through the standard library instead of through
`ffi/abi.go`.

The "chunks: 6" figure earlier in this README is a different measurement and
is not in tension with this one: it is the total chunk count of an
**uninterrupted** four-word stream from `-text "one two three four"` — four
content chunks, a finish-reason chunk, a usage chunk — from the plain
`run.sh direct`/`sidecar` walkthrough, which never cancels anything. Neither
number describes what the other measures.

### Per-call, not per-handle

`llmux_cancel(h)` in the C ABI is per-**handle**: it aborts every call
currently in flight on that gateway, so a binding that hands its users a
per-stream cancellation idiom (an `AbortSignal`, a `CancellationToken`) has to
warn that cancelling one stream can abort an unrelated stream sharing the same
handle, and recommend one gateway per cancellation scope where that matters.

Go's `context.Context` is per-**call**: it is a parameter to `gw.ChatStream`,
not state stored on `*gateway.Gateway`, so two concurrent calls on the same
gateway with two different contexts are independent by construction —
cancelling one's context never touches the other.
`TestChatStreamCancelIsPerCall` in `cancel_test.go` asserts exactly that: a
stream cancelled at 3 chunks runs alongside a second stream on the same
gateway that completes all 12 chunks, untouched. There is no "one gateway per
cancellation scope" caveat to write for Go, because a context already is a
cancellation scope.

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
  llmux/
    llmux.go        the convenience package (New, and the deprecated Start)
    cancel_test.go  proof that context cancellation reaches the upstream, for
                     both New/ChatStream and Start/Local
  examples/
    direct/         in-process gateway: models, chat, streaming chat, and
                     (-cancel-demo) context cancellation against fake-upstream.py
    sidecar/        spawn `llmux serve`, then HTTP + SSE against it
    run.sh          runs direct/sidecar/cancel offline, against
                     ffi/fakeupstream and sdks/fake-upstream.py respectively
  README.md
```

`sdks/go` has no `go.mod` of its own — it is part of the root
`github.com/vul-os/llmux` module, so `go test ./...` at the repo root covers it.

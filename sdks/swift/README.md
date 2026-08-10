# llmux for Swift

Two modes, both supported, both with a runnable example.

| | what it is | the type |
| --- | --- | --- |
| **Direct** | `dlopen`s `libllmux` and runs the gateway **inside your process** | `LLMux.Gateway` |
| **Sidecar** | spawns and supervises `llmux serve`, talks HTTP | `LLMux.Sidecar` |

**Direct is the better default on darwin/arm64, the only combination tested from
Swift.** A library also exists for linux/arm64, which should work but has not
been tried from Swift; on Intel macOS, Windows and iOS there is no library at
all, so direct mode does not exist there. See [Platforms](#platforms).

## Tested on

Everything below was executed on this machine, not inferred:

| | |
| --- | --- |
| Swift | Apple Swift **6.1.2** (swiftlang-6.1.2.1.2, clang-1700.0.13.5) |
| swift-driver | 1.120.5 |
| Target | `arm64-apple-macosx15.0` |
| macOS | **15.7.3** (build 24G419), Apple silicon |
| Xcode | **not installed** — Command Line Tools only. See [Testing](#testing) |
| Package | SwiftPM, tools-version 5.9, platform floor macOS 13 |
| llmux | 0.1.2 at first capture, whose `libllmux.dylib` was 12,769,346 bytes. Recaptured at **0.1.5** (adds `llmux_cancel`, the seventh C ABI symbol) for the [Cancellation](#cancellation) section below; today's darwin/arm64 build is 12,823,104 bytes — see [Platforms](#platforms) |

## Run the examples

Offline, with no provider keys and no network, against the OpenAI-compatible
fake in `ffi/fakeupstream`:

```
./sdks/swift/run.sh            # both
./sdks/swift/run.sh direct
./sdks/swift/run.sh sidecar
./sdks/swift/run.sh test
```

Real output:

```
==> direct (in-process, C ABI)
library: /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi:     0.1.5
models:  1580 bytes in 0.069ms
chat:    3.147ms
refusal: llmux: "stream": true is not valid for llmux_call; use llmux_stream
stream:  one two three four
         6 chunks, first at 1.936ms, total 2.926ms
early:   broke out after 2 chunk(s) — stopping is not an error
sync:    stopped after 3 chunk(s)
cancel:  consumer stopped at 3 chunk(s) (Task cancelled)
         upstream generated 3 of 12 (10 words + finish + usage chunk)
bogus:   llmux: unknown method "no-such-method" (want one of: chat, embed, models)

==> sidecar (child process over HTTP)
sidecar: http://127.0.0.1:58960
openai:  http://127.0.0.1:58960/v1
models:  1581 bytes in 1.797ms
chat:    2.981ms
stream:  one two three four
         6 chunks, first at 5.857ms, total 6.378ms
bogus:   HTTP 404: {"error":{"message":"no route for model \"no-such-model-anywhere\" ...
stopped: child reaped
```

Those timings are single cold calls from an example, not a benchmark. `ffi/bench`
is the measurement (~4 µs in-process against ~46 µs loopback over 1000
requests). Do not quote the example's numbers.

## Direct

```swift
import LLMux

let gw = try Gateway(configJSON: nil)        // nil = defaults + environment
let models = try gw.models()                 // JSON in, JSON out
let chat = try gw.chat(requestJSON)

for try await chunk in gw.chunks(streamingRequestJSON) {
    print(chunk)
}
```

**Closing is `deinit`.** `Gateway` owns the `UInt64` handle and releases it when
the last reference goes away — on the happy path, on a `throw`, on an early
`return`. ARC is the RAII here; there is no `close()` to forget and no `defer`
to write at the call site.

**Every returned string is freed.** Results and error messages alike go back
through `llmux_free` and nothing else. `Library.takeError` copies the message
into a Swift `String` and frees the original *before* the `Error` is
constructed — the step a hand-written binding usually misses, because it is easy
to forget that error strings are malloc'd exactly like results.
`llmux_abi_version` is the one exception: a static string that must **not** be
freed, and is not.

**Errors are `throws`,** with the library's own message in `LLMuxError.llmux`.
That message is plain UTF-8 text and deliberately not JSON — do not parse it.

**Cancellation** — `Gateway.cancel()`, and `chunks(_:)` wired to both breaking
a loop and `Task` cancellation — has its own section, [below](#cancellation).

### No module map, no `unsafeFlags`

The C ABI is reached with `dlopen`/`dlsym` and `@convention(c)` function types.
Three things follow, and all three matter:

- `swift build` works with nothing on the machine but a Swift toolchain — no
  header, no `-L`, no `-I`.
- The library is located at **run** time, so one build works whether `libllmux`
  sits in `dist/ffi/`, on `DYLD_LIBRARY_PATH`, or wherever `$LLMUX_LIBRARY`
  points.
- **This package can be a dependency of another package.** A target carrying
  `unsafeFlags` cannot be, which rules out the link-time approach for anything
  published — the usual reason a Swift C-interop package that "works locally"
  cannot be consumed.

Resolution order: `$LLMUX_LIBRARY`, then `dist/ffi/<goos>_<goarch>/` walking up
from the working directory, then the bare file name handed to the loader.

Probe the version at startup and refuse a mismatch — a shared library resolves
off a load path you may not control:

```swift
let gw = try Gateway(expectedVersion: "0.1.5")
```

### The library is loaded once and never unloaded

There is no `dlclose` in this SDK. `libllmux` is a Go `c-shared` object: loading
it starts the Go runtime and its threads, and Go has no way to shut that down,
so unloading would unmap code those threads are still executing.

The Rust binding for this same library was written the other way first and a
200-cycle open/close loop **hung** — each iteration slower than the last, until
the process had to be killed. `Library.shared(at:)` caches one instance per path
for the life of the process. `manyOpenCloseCyclesStayFast` is the guard.

### Streaming, and one honest limitation

`gw.chunks(_:)` is an `AsyncThrowingStream<String, Error>`, and
`gw.streamSync(_:onChunk:)` is the blocking callback form under it.

`llmux_stream` is a **blocking call with a callback**; an `AsyncSequence` is a
pull API. Bridging them needs somewhere for the blocking call to live, so
`chunks` dispatches it to `DispatchQueue.global` — **not** to Swift's
cooperative thread pool, because blocking one of those threads for the length of
a model's answer is how a Swift concurrency program deadlocks.

Chunks are yielded **as they arrive**, so time-to-first-token is real (1.936 ms
in the run above, against 2.926 ms total). But:

> **`AsyncThrowingStream` does not propagate backpressure to a non-async
> producer.** With `.unbounded` buffering, a consumer slower than the model
> queues chunks in memory. For a chat completion that is bounded by the answer
> length and is fine. It is not a general-purpose guarantee.

The Rust binding for the same ABI *does* get real backpressure, because
`sync_channel(0)` blocks the producer inside the callback until the consumer
takes the chunk. That is a difference in what the two languages' stream types
offer, not a difference in the ABI — and it is worth knowing before you build
something that assumes flow control.

The **sidecar's** stream does have real backpressure: `URLSession.bytes(for:)`
means the async `for` loop drives the socket read.

Breaking out of the loop, or cancelling the `Task`, terminates the stream.
Stopping early is **not** an error — see [Cancellation](#cancellation) for
what actually happens at the ABI when it does.

**Panics/traps.** Swift has no catchable panics, so there is no `catch_unwind`
equivalent in the trampoline (the Rust binding has one). A trap inside your
chunk callback terminates the process rather than unwinding through the Go call
frame — which is bad, but is at least not undefined behaviour. Keep the callback
simple.

## Cancellation

`llmux_cancel(h)`, added in 0.1.5, is the seventh symbol in the C ABI and the
only lever for abandoning a `llmux_call` or `llmux_stream` that is already
blocked — before it, the sole way out was `llmux_close`, which destroys the
gateway and every other call running on it. `Gateway.cancel()` binds it
directly:

```swift
gw.cancel()   // aborts every call in flight on gw, leaves the handle open
```

It is safe to call from another thread while a call on the same handle is
blocked, and safe from **inside a chunk callback** — unlike `llmux_close`,
which must never be called from one, because it waits (up to a few seconds)
for the very call that is running it. It is a no-op when nothing is running on
the handle, and a no-op the second time.

**`chunks(_:)` reaches it automatically.** `AsyncThrowingStream` calls
`onTermination` with `.cancelled` exactly when a consumer abandons a
sequence — breaking out of a `for try await` loop, or the consuming `Task`
being cancelled — and `chunks(_:)` calls `cancel()` right there, rather than
only setting a flag for the next chunk callback to notice. That distinction is
the whole fix: a flag only takes effect once another chunk *arrives* to check
it, and if the upstream has gone quiet between chunks, the blocking
`llmux_stream` call is stuck deep in a network read with nothing scheduled to
look at a flag. `llmux_cancel` is the one thing that reaches into that blocked
call from another thread and makes it return immediately.

**Measured**, against `sdks/fake-upstream.py --chunk-delay-ms 100 --text "one
two three four five six seven eight nine ten"` — ten words, 100 ms apart, so a
completed run generates 12 chunks (the ten words, a finish-reason chunk, and a
usage chunk): a consumer that stops after 3 chunks — whether by breaking the
loop (`breakingOutOfChunksStopsTheUpstream`) or by cancelling its `Task`
(`taskCancellationStopsTheUpstream`) — leaves the upstream having generated
**3 of those 12**. That is the `cancel:`/`generated:` pair in the run above,
and it is the number this whole feature exists to make true: before
`llmux_cancel`, a binding could stop reading locally while the provider ran to
completion and llmux metered every token of it, indistinguishable from the
outside.

**The error a live consumer sees is `CancellationError`, not `context
canceled`.** `chunks(_:)` catches `LLMuxError.llmux("context canceled")` —
Go's `context.Canceled.Error()`, produced only by a cancelled context; llmux's
own liveness timeouts (`stream_idle_timeout_seconds`,
`stream_first_byte_timeout_seconds`) return differently worded errors, so the
match is never ambiguous — and re-throws Swift's own `CancellationError`
instead. That translation is only *observable*, though, when something other
than the consuming `Task`'s own cancellation aborted the call:
`AsyncThrowingStream` resolves a `next()` suspended by the consuming task's
own cancellation to `nil`, ending the loop exactly like a normal completion,
without ever surfacing what the producer finished with. See
`taskCancellationStopsTheUpstream`, which asserts on the generated count and
deliberately not on a thrown error, versus
`explicitCancelOfAnActiveStreamThrowsCancellationError`, which calls
`gw.cancel()` from elsewhere while the consuming task is not itself cancelled
and does see `CancellationError` come out of the loop.

**Per-handle, not per-call — the caveat worth internalizing before relying on
this.** `llmux_cancel` aborts *every* call in flight on that gateway, not just
the one you meant to stop. Two `chunks(_:)` sequences sharing one `Gateway`
are not independently cancellable: breaking out of one aborts the other's call
too, mid-stream, and its consumer gets the same `CancellationError` even
though nothing asked to touch it. If you need independent cancellation scopes,
the fix is one `Gateway` per scope — the ABI has no finer-grained cancel than
the handle.

## Sidecar

```swift
let sc = try Sidecar()                       // spawns, waits for /health
let chat = try sc.chat(requestJSON)
for try await chunk in sc.chatStream(streamingRequestJSON) { print(chunk) }
sc.stop()
```

`deinit` stops the child, so an early `throw` cannot leave a serving llmux
holding a port. Binary resolution: `$LLMUX_BINARY`, then `bin/llmux` beside the
package, then `PATH`.

Two notes on the implementation, since both are patterns worth *not* copying
blindly:

- The non-streaming calls bridge `URLSession`'s callback API to a synchronous
  API with a `DispatchSemaphore`. Safe **here**, because the completion runs on
  a `URLSession` delegate queue and never on the thread being blocked. Do not
  copy it into code running on the cooperative pool.
- `freePort()` binds port 0, reads the port, and closes — inherently racy with
  anything else that might take it before the child binds. Every "find a free
  port" helper has this race, including the Go and Rust SDKs'; the alternative
  is passing the listening socket to the child, which llmux does not support.

## Testing

```
swift test
```

15 tests, all passing on the machine above — 4 of them the cancellation tests
described [above](#cancellation), added alongside `llmux_cancel`. The ones
needing a real `libllmux` are gated on it and **say which way they went**
rather than reporting a silent pass; the cancellation tests need `python3` on
`PATH` as well, to run `sdks/fake-upstream.py`, and skip themselves just as
honestly when it is absent:

```
libllmux found at …/dist/ffi/darwin_arm64/libllmux.dylib — direct tests RAN
✔ Test run with 15 tests passed after 0.269 seconds.
```

**The suite uses swift-testing (`import Testing`), not XCTest, and that was
forced rather than chosen.** XCTest ships with Xcode; it is **not** in the
Command Line Tools. On a CLT-only machine `import XCTest` fails with "no such
module 'XCTest'" and `swift test` cannot build at all. swift-testing is part of
the Swift 6 toolchain, so it works with the CLT alone. If you are packaging a
Swift SDK, this is worth knowing before you write a suite that only runs on
machines with a 10 GB IDE installed.

## Platforms

**Do not read one matrix for llmux and openrate — they differ.** This is
llmux's:

| target | status | Swift? |
| --- | --- | --- |
| darwin/arm64 | built, smoke-tested (40/40 C checks), 12,823,104 bytes | **yes — this is the tested combination** |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container, 17,356,264 bytes | should work; not tried here |
| linux/amd64 | CI only, never built locally | untested from Swift |
| darwin/amd64 | **not built** | **direct mode unavailable** |
| windows/amd64 | **not built — no `llmux.dll` exists** | **direct mode unavailable** |

Intel Macs, Windows, iOS, and every Apple embedded platform therefore have no
direct mode. `LibraryLocator.fileName` names `llmux.dll` for completeness; that
is not a promise anyone ships. On those platforms use the sidecar — `llmux` is a
plain Go binary and cross-compiles anywhere — or, on iOS specifically, talk to a
remote `llmux serve` over the network, since iOS does not permit loading an
arbitrary `dlopen`'d dylib or spawning child processes at all.

## The costs of direct mode

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   signal handlers. Measured: Go replaces `SIGSEGV`, `SIGBUS`, `SIGFPE`,
   `SIGPIPE` and `SIGURG`, and adds `SA_ONSTACK` to `SIGILL`, `SIGXFSZ` and
   `SIGUSR2`. The Swift runtime does not install competing handlers, so this is
   quieter for Swift than for a JVM; but a crash reporter or a sanitizer build in
   the same process can still conflict. **`SIGPROF` is not touched**, and
   Instruments samples out of process, so profiling is unaffected either way. The
   measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).
2. **Not fork-safe.** After `fork()` without `exec()` the Go runtime in the
   child is broken. Swift's `Foundation.Process` always `exec`s, and there is no
   idiomatic Swift pre-fork worker model, so the practical victims are narrow:
   direct `fork(2)` in your own C interop. If you do fork, load the library
   **after** the fork, in the child.
3. **The shared library is 12–17 MB**, which on a distributed macOS app is a
   real number and on iOS is moot because you cannot ship it at all.
4. **Prebuilt for two targets.** See above.
5. **Latency is not the reason to embed.** ~4 µs against ~46 µs for the
   boundary; a real model answers in hundreds of milliseconds. The reasons are
   no second process to supervise, no port to bind, and no loopback surface to
   secure.
6. **When the sidecar is the better answer for Swift:** any platform except
   darwin/arm64 and linux/arm64; per-tenant virtual keys and budgets (enforced
   by the HTTP shell's auth middleware, which an in-process caller sits inside
   of and bypasses by construction); several processes sharing one gateway and
   one cache; wanting llmux restartable independently of your app.

## Layout

```
sdks/swift/
  Package.swift                        no dependencies, no unsafeFlags
  Sources/LLMux/Direct.swift           the C ABI binding — Gateway, cancel(), deinit, AsyncSequence
  Sources/LLMux/Sidecar.swift          spawn/supervise `llmux serve`, URLSession + SSE
  Sources/llmux-direct-example/        also spawns sdks/fake-upstream.py for the cancellation demo
  Sources/llmux-sidecar-example/
  Tests/LLMuxTests/DirectTests.swift   swift-testing; gated tests announce themselves
  run.sh                               runs the examples offline against ffi/fakeupstream
```

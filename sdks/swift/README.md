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
| llmux | 0.1.2, `libllmux.dylib` 12,769,346 bytes |

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
abi:     0.1.2
models:  1580 bytes in 0.103ms
chat:    4.409ms
refusal: llmux: "stream": true is not valid for llmux_call; use llmux_stream
stream:  one two three four
         6 chunks, first at 1.159ms, total 2.037ms
early:   broke out after 2 chunk(s) — stopping is not an error
sync:    stopped after 3 chunk(s)
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
let gw = try Gateway(expectedVersion: "0.1.2")
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

Chunks are yielded **as they arrive**, so time-to-first-token is real (1.159 ms
in the run above, against 2.037 ms total). But:

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

Breaking out of the loop, or cancelling the `Task`, terminates the stream: the
callback sees the termination flag and returns non-zero, which stops it at the
library. Stopping early is **not** an error.

**Panics/traps.** Swift has no catchable panics, so there is no `catch_unwind`
equivalent in the trampoline (the Rust binding has one). A trap inside your
chunk callback terminates the process rather than unwinding through the Go call
frame — which is bad, but is at least not undefined behaviour. Keep the callback
simple.

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

11 tests, all passing on the machine above. The ones needing a real `libllmux`
are gated on it and **say which way they went** rather than reporting a silent
pass:

```
libllmux found at …/dist/ffi/darwin_arm64/libllmux.dylib — direct tests RAN
✔ Test run with 11 tests passed after 0.032 seconds.
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
| darwin/arm64 | built, smoke-tested (32/32 C checks), 12,787,504 bytes | **yes — this is the tested combination** |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container, 17,348,392 bytes | should work; not tried here |
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
  Sources/LLMux/Direct.swift           the C ABI binding — Gateway, deinit, AsyncSequence
  Sources/LLMux/Sidecar.swift          spawn/supervise `llmux serve`, URLSession + SSE
  Sources/llmux-direct-example/
  Sources/llmux-sidecar-example/
  Tests/LLMuxTests/DirectTests.swift   swift-testing; gated tests announce themselves
  run.sh                               runs the examples offline against ffi/fakeupstream
```

# llmux (Kotlin)

Idiomatic Kotlin over the [Java SDK](../java): `use {}`, named arguments,
default parameters, and a coroutine [`Flow`] for streaming.

| | class | what it is | recommended? |
|---|---|---|---|
| **Sidecar** | `LlmuxSidecar` | spawns `llmux serve` on `127.0.0.1`, talks HTTP | **yes — the default for the JVM** |
| **Direct** | `Llmux.direct()` → `LlmuxGateway` | loads `libllmux` into this JVM | only after reading the [Java SDK's signal-handler section](../java/README.md#the-jvm-and-gos-signal-handlers) |

**This is a wrapper, not a reimplementation.** The FFM binding, the memory
rules, the handle lifecycle and the signal-handler measurements all live in
`sdks/java`. Kotlin adds ergonomics on top. That is deliberate: two bindings to
one C ABI is two places for a use-after-free.

```sh
sdks/kotlin/run-examples.sh            # direct + sidecar
sdks/kotlin/run-examples.sh direct
sdks/kotlin/run-examples.sh sidecar
sdks/kotlin/run-examples.sh cancel     # llmux_cancel, against sdks/fake-upstream.py
```

The script builds the shared library and the gateway binary, starts the fake
upstream from `ffi/fakeupstream`, and runs the examples against it — no API
key, no network. `cancel` is its own mode: cancelling mid-stream needs an
upstream with a chunk delay and a generation counter, which `ffi/fakeupstream`
does not have — see [Cancellation](#cancellation).

---

## Sidecar — the recommended default

```kotlin
LlmuxSidecar().use { llmux ->
    println(llmux.openAiBaseUrl)          // http://127.0.0.1:<port>/v1
    println(llmux.chat(requestJson))
    llmux.chatChunks(streamRequestJson).collect { print(it) }
}
```

`use {}` stops the child process on every path out, including a thrown
exception, rather than waiting for the JVM shutdown hook. Runs on **Java 11+**
with no native library, no `--enable-native-access`, and no platform matrix —
including on Windows, where the direct path does not exist at all.

Worked example: [`examples/SidecarChat.kt`](examples/SidecarChat.kt).

## Direct — in-process

```kotlin
Llmux.direct(configJson).use { llmux ->
    println(llmux.abiVersion)
    println(llmux.call("models"))
    llmux.chunks(requestJson = streamRequestJson).collect { print(it) }
}
```

Requires **Java 22+** (the underlying binding is `java.lang.foreign`) and
`--enable-native-access=ALL-UNNAMED`. Worked examples:
[`examples/DirectChat.kt`](examples/DirectChat.kt) (calls and streaming) and
[`examples/CancelChat.kt`](examples/CancelChat.kt) (cancellation — see below).

### The C ABI surface: seven symbols

The Java SDK this wraps binds every function `ffi/include/llmux.h` declares:
`llmux_abi_version`, `llmux_new`, `llmux_close`, `llmux_cancel`, `llmux_call`,
`llmux_free`, `llmux_stream`. `llmux_cancel` is the newest, added in llmux
0.1.5; before it, `close()` was the only way out of a blocked call, and it
takes the whole handle down with it. [`LlmuxGateway.cancel()`](src/main/kotlin/to/llmux/kotlin/Direct.kt)
is the thin Kotlin delegate for it, and [`chunks()`](#cancellation) is wired to
reach it on its own.

### `chunks()` is a rendezvous, and that is the point

The direct stream is a `Flow<String>` of `chat.completion.chunk` documents.
Cancelling the collector stops the Go stream: the send from the callback fails,
the callback returns "stop", and `llmux_stream` unwinds.

**That only works with backpressure, and getting it wrong is invisible.**
Measured on a five-chunk stream collected with `take(2)`:

| flow capacity | chunks collected | times the C callback actually fired |
|---|---|---|
| `Channel.BUFFERED` (64) | 2 | **5 — the whole stream ran** |
| `Channel.RENDEZVOUS` (0) | 2 | **3 — two collected, one in flight** |

With a buffer, the producer fills 64 slots long before the collector cancels, so
`take(2)` silently discards three chunks that were already generated, streamed
and metered. The Flow looks identical from the outside in both cases; only a
counter inside the callback tells them apart. `chunks()` therefore uses
`Channel.RENDEZVOUS`. The cost is a context switch per chunk, which is nothing
against a model emitting tokens.

Cancellation is prompt, not retroactive: expect one chunk beyond your last
collected one — or it was, before `llmux_cancel` existed. See below for what
changed.

The sidecar's `chatChunks()` cancels by closing the response body. How much the
server had already produced is **not observable from the client** — the exact
callback count above is a property the direct path has and the sidecar does not.

### Cancellation

`take(2)` above is the Flow completing *on its own terms* — the collector
decided it had enough. This section is about the opposite case: something
*outside* the collection decides to stop it — a coroutine cancelling another,
a scope shutting down, a `withTimeout` expiring — and what that needs to reach
is `llmux_cancel`, not just "the collector stopped asking".

```kotlin
// Cancelling the collecting coroutine directly:
val job = launch { llmux.chunks(requestJson = streamRequestJson).collect { print(it) } }
// ... from elsewhere ...
job.cancel()

// Or a deadline around the collection:
withTimeout(500) {
    llmux.chunks(requestJson = streamRequestJson).collect { print(it) }
}
```

Both reach `llmux_cancel` — `chunks()`'s `awaitClose` calls
[`LlmuxGateway.cancel()`](src/main/kotlin/to/llmux/kotlin/Direct.kt) itself
rather than waiting for the next chunk to notice the channel closed, so a
provider that is *between* chunks (no callback pending) still gets cut off
promptly instead of running until it next comes up for air. And both throw
[`CancellationException`](https://kotlinlang.org/api/kotlinx.coroutines/kotlinx-coroutines-core/kotlinx.coroutines/-cancellation-exception/)
out of `collect` — `withTimeout`'s own `TimeoutCancellationException` is a
subtype — never a wrapped `LlmuxException` about the "context canceled" that
`llmux_stream` fails with underneath. Getting that second part right needed
code, not just relying on it: the cancellation `chunks()` triggers on its own
behalf, via its own `cancel()` call, comes back up through the exact same
catch block as a genuine external failure would, so the two are told apart
with a flag set before `cancel()` is called, not inferred after the fact.

**Measured**, with `sdks/fake-upstream.py --chunk-delay-ms 100 --text "one two
three four five six seven eight nine ten"` (ten words, one chunk each, plus a
trailing usage frame the harness does not count — 12 chunks run to
completion), cancelling once the collector had seen 3:

| construct | consumer saw | upstream `generated` |
|---|---|---|
| `job.cancel()` | 3 | **3** |
| `withTimeout(350)` | 3 | **3** |

Both stop the provider well short of the 12 it takes to finish, and both leave
the handle open — `examples/CancelChat.kt` calls `models` again afterward to
prove it. Reproduce it with:

```sh
sdks/kotlin/run-examples.sh cancel
```

Contrast this with the [Java SDK's own cancellation measurement](../java/README.md#cancellation):
there, interrupting the blocked thread (the idiomatic Java construct) is one
chunk looser than calling `cancel()` directly, because a blocking native call
can only notice a Java interrupt flag on its next chunk callback. Kotlin does
not have that gap here, because `chunks()`'s `awaitClose` calls `cancel()`
itself the instant the coroutine machinery decides to cancel — it does not
wait for the blocked IO thread to notice anything.

**`llmux_cancel` is per-HANDLE, not per-call.** It aborts *every* call
currently in flight on that gateway — a concurrent `call()`, `stream()`, or a
sibling `chunks()` collection on the very same `LlmuxGateway` is aborted too,
not just the one being cancelled. If cancelling one collection must not touch
another running on the same object, give it its own gateway (`Llmux.direct()`
again) rather than sharing one.

---

## The JVM and Go's signal handlers

Kotlin runs on the JVM, so this applies unchanged, and it is the reason the
sidecar is the recommended default. It is measured, not asserted — the numbers,
the `-Xcheck:jni` output, the `libjsig` result and the verdict are in
[`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers), and
the probe that produces them is `sdks/java/signal-probe.sh`.

The one-paragraph version: loading `libllmux` replaces the JVM's `SIGSEGV`,
`SIGBUS`, `SIGFPE`, `SIGPIPE` and `SIGURG` handlers and adds `SA_ONSTACK` to
three more; `SIGPROF` is untouched, so profiling is fine; both runtimes keep
working because Go chains; HotSpot nonetheless reports its handlers as modified
under `-Xcheck:jni` and tells you to preload `libjsig`, which fixes it — but
`libjsig` is a flag on the **java launch command**, and a library cannot add one
to a process that has already started. A dependency that requires changing how
the JVM is launched is not a drop-in.

## The other costs of the direct path

Full detail in [`ffi/README.md`](../../ffi/README.md); the Java-specific
treatment is in [`sdks/java/README.md`](../java/README.md#the-costs-of-the-direct-path).

1. **The Go runtime is in your process** — see above.
2. **Not fork-safe.** After `fork()` without `exec()` the Go runtime in the
   child is broken. The JVM is a mild case: it does not pre-fork, and
   `ProcessBuilder` / `Runtime.exec` are safe because they `posix_spawn` or
   `fork`+`exec`. The victims are a JNI/FFM call to bare `fork(2)`, and any
   supervisor that forks the JVM after the library is loaded.
3. **12–17 MB of shared library** — 12,823,104 bytes on darwin/arm64.
4. **Two platforms, and neither is Windows.** See below.
5. **Latency is not the reason.** ~4 µs in-process versus ~46 µs over loopback
   for the boundary; ~80–92 µs versus ~102–109 µs for a chat call — noise
   against a model taking hundreds of milliseconds. The reasons are no second
   process, no port, no loopback surface, and an inert library that opens no
   sockets until you call it.

## Platforms (direct path only)

| target | status |
|---|---|
| darwin/arm64 | built and smoke-tested. 12,823,104 bytes. Everything here was run on it. |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,356,264 bytes. |
| linux/amd64 | CI only. |
| **windows/amd64** | **not built. No DLL exists.** |
| **darwin/amd64** | **not built.** |

**Windows has no shared library**, and this file will not give install
instructions for one. On Windows, use the sidecar; it is an ordinary Go binary
and works there.

## Dependencies, and the absence of a Gradle build

One dependency: **`org.jetbrains.kotlinx:kotlinx-coroutines-core`** (1.10.2),
for `Flow`. It is not avoidable — `Flow` is the deliverable — and it is the
only one. The Java SDK it wraps has none.

There is **no `build.gradle.kts` here**, on purpose. Nothing in this repo runs
Gradle, so a build file would be an unexecuted claim about how the module
builds, and this repo's standard is that a check nobody runs is worse than no
check. `run-examples.sh` drives `kotlinc` directly, resolving the coroutines jar
from the local Maven repository (fetching it with `mvn dependency:get` if
absent), and it is run for real. When the SDK is published, consumers will
depend on the artifact rather than this tree:

```kotlin
dependencies {
    implementation("to.llmux:llmux-kotlin:0.1.0")       // not yet published
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.10.2")
}
```

## Toolchain this was built and run on

- OpenJDK **26.0.2** (Homebrew), darwin/arm64
- Kotlin **2.4.10** (`kotlinc-jvm`), `-jvm-target 22`
- kotlinx-coroutines-core-jvm **1.10.2**
- Go **1.25.12**, llmux **0.1.5** (the `llmux_cancel` measurements above are
  from this version; earlier sections of this file predate it)

`-jvm-target 22` is a floor, not a preference: `to.llmux.LlmuxDirect` is a Java
22 class file and kotlinc must be able to read it.

## Provider keys

Inherited from the environment (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, …) on both paths.

## Layout

```
sdks/kotlin/
  src/main/kotlin/to/llmux/kotlin/Direct.kt    LlmuxGateway, Llmux.direct(), cancel(), chunks(): Flow
  src/main/kotlin/to/llmux/kotlin/Sidecar.kt   LlmuxSidecar, chatChunks(): Flow
  examples/DirectChat.kt                       runnable
  examples/SidecarChat.kt                      runnable
  examples/CancelChat.kt                       runnable — llmux_cancel, see Cancellation
  run-examples.sh                              builds everything, runs direct/sidecar/cancel
```

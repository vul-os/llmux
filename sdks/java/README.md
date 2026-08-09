# llmux (Java)

Two ways to run llmux from Java, both supported:

| | class | what it is | recommended? |
|---|---|---|---|
| **Sidecar** | `to.llmux.Llmux` | spawns `llmux serve` on `127.0.0.1`, hands you a base URL | **yes — the default for the JVM** |
| **Direct** | `to.llmux.LlmuxDirect` | loads `libllmux` and runs the gateway *inside this JVM* | only after reading [the signal-handler section](#the-jvm-and-gos-signal-handlers) |

**On the JVM, the sidecar is the recommended default.** That is not a hedge and
it is not because the direct path is broken — it works, both examples below run
green, and the measurements are in this file. It is because the direct path
requires a change to the *java launch command* that a library cannot make on
its own. See [the verdict](#the-verdict).

Run both examples:

```sh
sdks/java/run-examples.sh            # both
sdks/java/run-examples.sh direct
sdks/java/run-examples.sh sidecar
```

The script builds the shared library, builds the gateway binary, starts the
fake OpenAI upstream from `ffi/fakeupstream`, and runs the examples against it —
so neither needs an API key or a network.

---

## Sidecar — `to.llmux.Llmux`

The gateway as a child process. Point any OpenAI-compatible client at the URL.

```java
import to.llmux.Llmux;

String base = Llmux.baseUrl();        // http://127.0.0.1:<port>
String v1   = Llmux.openaiBaseUrl();  // http://127.0.0.1:<port>/v1
```

It starts lazily on first use, is reused (singleton), and is terminated by a JVM
shutdown hook. `Llmux.stop()` ends it earlier; the example does that in a
`finally` so a failure does not leave a child process behind.

Convenience with the optional `com.openai:openai-java` dependency:

```java
OpenAIClient client = OpenAIOkHttpClient.builder()
    .baseUrl(Llmux.openaiBaseUrl())
    .apiKey("llmux-local")
    .build();
```

**Requires Java 11.** No native library, no FFM, no signal handlers, no
platform matrix — the binary is a separate process and the JVM never learns
what it is written in.

Worked example: [`examples/SidecarChat.java`](examples/SidecarChat.java) —
models, one completion, one SSE stream, and the error path, using only
`java.net.http`.

### Binary resolution

1. `LLMUX_BINARY`
2. bundled `bin/llmux` (a sibling `bin/` next to the jar/classes, or
   `LLMUX_HOME/bin/llmux`)
3. `llmux` on `PATH`

```sh
go build -o sdks/java/bin/llmux ./cmd/llmux     # or: make sdk-bins
```

---

## Direct — `to.llmux.LlmuxDirect`

llmux inside your JVM, through the C ABI documented in
[`ffi/README.md`](../../ffi/README.md). Read that first; this section covers
only what is specific to Java.

```java
try (LlmuxDirect llmux = LlmuxDirect.open(configJson)) {
    String models = llmux.call("models", null);
    String answer = llmux.call("chat", requestJson);

    llmux.stream("chat", streamRequestJson, chunk -> {
        System.out.print(chunk);
        return true;                 // false stops the stream; not an error
    });
}
```

JSON in, JSON out — the same JSON `POST /v1/chat/completions` takes and
returns. The binding does not parse it and has no JSON dependency; use whatever
parser you already have.

Worked example: [`examples/DirectChat.java`](examples/DirectChat.java).

### Requirements

- **Java 22+.** `java.lang.foreign` (the FFM API) became permanent in Java 22.
  **Tested on OpenJDK 26.0.2 (Homebrew), darwin/arm64.**
- **`--enable-native-access=ALL-UNNAMED`** on the java command line. Without it
  JDK 24+ prints a warning on the first restricted call and a later release will
  throw.
- A `libllmux` for your platform — see [Platforms](#platforms).

### Why FFM and not JNA

FFM is in the JDK. JNA is a dependency, and one that ships its own native stub
per platform, so adopting it to load a native library means shipping *two*
native artifacts to solve the problem of shipping one. FFM also gives real
upcall stubs with a defined exception policy, which `llmux_stream` needs.

**JNA is the documented fallback for Java 11–21**, where FFM is unavailable or
preview-only. That path is *not implemented here and not tested*; if you need
it, the shape is `Pointer llmux_call(long, String, String, PointerByReference)`
with `Native.load("llmux", ...)`, and the two things to get right are (a) map
returns as `Pointer`, never `String`, or JNA will copy and leak the C
allocation instead of passing it back to `llmux_free`, and (b) use
`Callback` with an `int` return for the chunk callback. Given that Java 21 is an
LTS in wide use, **the honest recommendation for Java 11–21 is the sidecar**,
not a JNA binding — the sidecar is fully supported there and needs no native
code at all.

### Memory and lifetime

Everything the library returns is freed with `llmux_free` and nothing else. The
binding does this for you:

- **results** are copied into a `java.lang.String`, then freed in a `finally`;
- **error strings** are read, freed, and turned into `LlmuxException`;
- **the `char** err` out-parameter is drained even on the success path**, so a
  library that sets a message alongside a success cannot leak it;
- **handles** are closed by `close()`, which is idempotent — use
  try-with-resources, as both examples do. Use after close is a clean
  `LlmuxException`, never a crash.

The shared library itself is loaded into `Arena.global()` and is **never
unloaded**. `dlclose` on a library carrying the Go runtime is not an operation
to attempt.

### Streaming, and the one rule that matters

`stream()` invokes your handler once per chunk **on the calling thread**,
synchronously, before `stream()` returns. That is measured, not assumed —
`ffi/ctest/smoke.c` compares `pthread_self()` on every CI run, and
`DirectChat.java` re-checks it with `Thread.currentThread()`.

The rule: **an exception must never escape an FFM upcall stub** — it terminates
the JVM. `LlmuxDirect` catches everything your handler throws, records it,
returns "stop" to llmux so the stream unwinds cleanly, and rethrows it from
`stream()` once control is back on the Java side. Write your handler normally;
the binding holds that line for you.

---

## The JVM and Go's signal handlers

This is the honest requirement that matters most for Java, so it is measured
rather than described. Reproduce it:

```sh
sdks/java/signal-probe.sh              # what changed
sdks/java/signal-probe.sh --checkjni   # HotSpot's own audit of what changed
sdks/java/signal-probe.sh --jsig       # again, with libjsig preloaded
```

[`tools/SignalHandlerProbe.java`](tools/SignalHandlerProbe.java) reads every
interesting `sigaction` handler, loads `libllmux`, reads them again, and then
tries to provoke the JVM into using the ones that changed.

### What actually happens

On **OpenJDK 26.0.2, darwin/arm64, llmux 0.1.2**:

```
signal    before                after                 verdict
--------------------------------------------------------------------------
SIGILL    0x101f5fcc0 f=0x42    0x101f5fcc0 f=0x43    flags changed (Go added SA_ONSTACK)
SIGFPE    0x101f5fcc0 f=0x42    0x1364a93d0 f=0x43    HANDLER REPLACED by the Go runtime
SIGBUS    0x101f5fcc0 f=0x42    0x1364a93d0 f=0x43    HANDLER REPLACED by the Go runtime
SIGSEGV   0x101f5fcc0 f=0x43    0x1364a93d0 f=0x43    HANDLER REPLACED by the Go runtime
SIGPIPE   0x101f5fcc0 f=0x42    0x1364a93d0 f=0x43    HANDLER REPLACED by the Go runtime
SIGURG    SIG_DFL f=0x0         0x1364a93d0 f=0x43    HANDLER REPLACED by the Go runtime
SIGXFSZ   0x101f5fcc0 f=0x42    0x101f5fcc0 f=0x43    flags changed (Go added SA_ONSTACK)
SIGPROF   SIG_DFL f=0x0         SIG_DFL f=0x0         unchanged
SIGUSR2   0x101ebb940 f=0x42    0x101ebb940 f=0x43    flags changed (Go added SA_ONSTACK)

5 handler(s) replaced, 3 left in place with altered flags
```

Five points, each of which contradicts something that gets assumed:

1. **`SIGSEGV` is replaced.** `0x101f5fcc0` is HotSpot's `javaSignalHandler`;
   `0x1364a93d0` is `runtime.cgoSigtramp` in `libllmux.dylib`. HotSpot elides
   null checks in compiled code and recovers them from `SIGSEGV`, and it grows
   stacks through guard-page faults. This is the JVM's most load-bearing signal
   and a Go shared library takes it over.

2. **`SIGPROF` is *not* touched.** This is the one everybody expects to break,
   and it does not. Go's `sigInstallGoHandler` refuses to install anything but
   *synchronous* signals plus `SIGPIPE` and `SIGURG` when built
   `-buildmode=c-shared`, and `SIGPROF` is neither. A JFR recording taken while
   `libllmux` was loaded collected **428 `jdk.ExecutionSample` events**, i.e.
   profiling works. Do not write defensive code for this.

3. **Go mutates handlers it does not replace.** `SIGILL`, `SIGXFSZ` and
   `SIGUSR2` keep HotSpot's function pointer but gain `SA_ONSTACK`. `SIGUSR2` is
   HotSpot's `SR_handler` — thread suspend/resume, which safepoints and stack
   walking depend on. Go is editing the flags of the JVM's own machinery.

4. **HotSpot notices.** With `-Xcheck:jni`, the VM audits its handlers
   periodically and says so itself:

   ```
   Warning: SIGSEGV handler modified!
   Warning: SIGILL handler modified!
   Warning: SIGFPE handler modified!
   Warning: SIGBUS handler modified!
   Warning: SIGUSR2 handler modified!
   ...
      SIGSEGV: runtime.cgoSigtramp.abi0 in libllmux.dylib, flags=SA_ONSTACK|SA_RESTART|SA_SIGINFO
     *** Handler was modified!
     *** Expected: javaSignalHandler in libjvm.dylib
   Consider using jsig library.
   ```

5. **And yet both runtimes keep working.** Go chains: its handler forwards a
   signal that did not arrive in Go code to the handler it displaced. Measured
   after loading the library, on the same JVM:

   - 2,000,000 implicit null checks raised and recovered as `NullPointerException`;
   - `StackOverflowError` raised and caught;
   - a Go nil-pointer dereference *inside* the library, called from a Java
     thread, recovered as a Go panic — three times in a row, with the JVM's
     null checks still working afterwards. Chaining works in **both**
     directions, not just JVM-ward.

### `libjsig` is the supported fix, and it works

The JDK ships `libjsig`, whose entire job is this problem: it interposes
`sigaction` so a library installing handlers gets chained behind the JVM's
instead of over them. Preloading it removes every warning above:

```sh
# macOS
DYLD_INSERT_LIBRARIES=$JAVA_HOME/lib/libjsig.dylib java -Xcheck:jni …
# Linux
LD_PRELOAD=$JAVA_HOME/lib/libjsig.so java …
```

With it preloaded, `-Xcheck:jni` reports **no modified handlers at all**, and
the probe's functional checks still pass.

One measurement caveat, because it is the kind of thing that turns into a false
finding: **`libjsig` interposes the probe's own `sigaction()` calls too**, so
under `--jsig` the address column still shows Go's handler. That column is not
evidence there. HotSpot's `-Xcheck:jni` audit is the authority, and it is
silent.

### The verdict

The direct path on the JVM is **not broken, and not safe to adopt silently**.

- It works, in both directions, on the platform measured here.
- HotSpot's own diagnostics report its handlers as replaced, which means you
  have opted out of a guarantee the VM makes about its own crash handling —
  including whether you get a usable `hs_err_pid` file when something else goes
  wrong.
- The fix is real, supported, and one line — but it is a line on the **java
  launch command** (`LD_PRELOAD` / `DYLD_INSERT_LIBRARIES`). A library cannot
  add it to the process that already started. **A dependency that requires
  changing how the JVM is launched is not a drop-in**, and shipping one as the
  default would mean most users run in the unaudited configuration without
  knowing it.
- Only two platforms have a library at all, and neither is Windows — a real
  problem for a Java ecosystem where Windows is a first-class deployment target.
- The measured latency win is ~40 µs per call against a model that answers in
  hundreds of milliseconds. It is not a reason.

**So: use the sidecar unless you have a specific reason not to.** Good reasons
exist — no second process to supervise, no port to bind, no loopback surface —
and if one of them applies, use `LlmuxDirect`, preload `libjsig`, and run
`signal-probe.sh` on your own JDK and platform first. Choosing the sidecar is
the supported outcome of reading this page, not a failure.

### Untested, and stated as such

- **Linux.** Everything above is darwin/arm64. Signal numbers differ, glibc
  differs, and profilers that use `SIGSEGV`-based tricks are far more common
  there. `signal-probe.sh` knows the Linux signal numbers and will run; nobody
  has run it.
- **JVMTI agents and async-profiler.** An agent that installs its own handlers
  *after* `libllmux` loads captures Go's handler as the one to chain to. If it
  chains, the three-way chain works; if it does not, it breaks both. Untested.
- **JDKs other than 26.** FFM is stable from 22; the signal behaviour is
  HotSpot's and is unlikely to differ much, but "unlikely" is the honest word.

---

## The costs of the direct path

Not a footnote. Full detail in [`ffi/README.md`](../../ffi/README.md).

1. **The Go runtime is in your process** — GC, scheduler, signal handlers. See
   the whole section above; this is the big one for Java.
2. **Not fork-safe.** After `fork()` without `exec()` the Go runtime in the
   child is broken. On the JVM this is a much smaller hazard than it is for
   Python or Ruby, because the JVM does not pre-fork and there is no `fork`
   start method to trip over. The concrete Java victims are:
   `Runtime.exec` / `ProcessBuilder` are **safe** (they `fork`+`exec`, or use
   `posix_spawn`, which is the default on modern JDKs); a **JNI or FFM call to
   `fork(2)` that then runs Java or llmux code without `exec`** is not; and
   neither is a container init or supervisor that forks the JVM after the
   library is loaded. If you use `-Djdk.lang.Process.launchMechanism=fork`,
   note it is still `fork`+`exec` and therefore fine.
3. **The library is 12–17 MB** — 12,787,504 bytes on darwin/arm64.
4. **Platform coverage is narrow.** See below.
5. **Latency is not the reason to embed.** ~4 µs in-process versus ~46 µs over
   loopback for the boundary itself; ~80–92 µs versus ~102–109 µs for a real
   chat call. Against a model taking hundreds of milliseconds that is noise.
   The reasons are no second process, no port, and no loopback surface.

One difference you can see in the examples' own output: the **sidecar starts
background price-catalog syncing** (it logs `pricing source openrouter: 400
models` and reaches out to `raw.githubusercontent.com`), while the **direct path
is inert** — `llmux_new` starts no goroutines and opens no sockets. If "no
background traffic I did not ask for" is what you are buying, that is a real
argument for direct, and a better one than latency.

## Platforms

For the **shared library** (the direct path only):

| target | status |
|---|---|
| darwin/arm64 | built and smoke-tested. 12,787,504 bytes. The measurements on this page are from here. |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,348,392 bytes. |
| linux/amd64 | built and tested in CI only; not produced on a development machine. |
| **windows/amd64** | **not built. No DLL exists. Nobody has produced one.** |
| **darwin/amd64** | **not built.** |

**There is no Windows support for the direct path today**, and this file will
not pretend otherwise by giving you install instructions for a DLL that does not
exist. Windows users: the sidecar builds and runs there like any other Go
binary, and it is the answer.

The **sidecar** has no such matrix — it is a normal Go binary.

## Provider keys

Inherited from the environment (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, …) on both paths.

## Layout

```
sdks/java/
  src/main/java/to/llmux/Llmux.java            sidecar (Java 11+)
  src/main/java/to/llmux/LlmuxDirect.java      direct, FFM (Java 22+)
  src/main/java/to/llmux/LlmuxException.java
  examples/SidecarChat.java                    runnable
  examples/DirectChat.java                     runnable
  tools/SignalHandlerProbe.java                the evidence for this README
  run-examples.sh                              builds everything, runs both
  signal-probe.sh                              runs the probe
  run-java-check.sh                            the dependency-free smoke test
```

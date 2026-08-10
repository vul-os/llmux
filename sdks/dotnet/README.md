# llmux (.NET)

Two ways to run llmux from C#, both supported:

| | type | what it is | recommended? |
|---|---|---|---|
| **Sidecar** | `Llmux.Sidecar` | spawns `llmux serve` on `127.0.0.1`, talks HTTP | **yes — the default for .NET** |
| **Direct** | `Llmux.LlmuxDirect` | loads `libllmux` and runs the gateway *in this process* | only if you are not shipping to Windows |

**For .NET the sidecar is the recommended default, and the deciding reason is
platform coverage: there is no Windows shared library.** Not "not tested" —
**not built, by anyone, ever**. .NET has a very large Windows install base, so a
direct-mode dependency would be a library that does not load for a large
fraction of the people who take it. The sidecar is an ordinary Go binary and
runs everywhere.

Run both examples:

```sh
sdks/dotnet/run-examples.sh            # both
sdks/dotnet/run-examples.sh direct
sdks/dotnet/run-examples.sh sidecar
```

The script builds the shared library, builds the gateway binary, starts the fake
upstream from `ffi/fakeupstream`, starts a second fake — `sdks/fake-upstream.py`,
which adds a per-chunk delay and a `GET /generated` counter — for the direct
example's cancellation demo, and runs the examples against them: no API key and
no network.

---

## Sidecar — `Llmux.Sidecar`

```csharp
using Llmux;

string baseUrl = Sidecar.BaseUrl();        // http://127.0.0.1:<port>
string v1      = Sidecar.OpenAIBaseUrl();  // …/v1
```

Starts lazily on first use, is reused (singleton), and is terminated on
`ProcessExit` / Ctrl-C. `Sidecar.Stop()` ends it sooner — the example calls it
in a `finally` so a failure does not orphan a child process.

Point the official `OpenAI` nuget at it:

```csharp
var client = new OpenAIClient(
    new System.ClientModel.ApiKeyCredential("llmux-local"),
    new OpenAIClientOptions { Endpoint = new Uri(Sidecar.OpenAIBaseUrl()) });
```

Worked example: [`examples/SidecarChat.cs`](examples/SidecarChat.cs) — models,
one completion, an SSE stream as `IAsyncEnumerable<string>`, early cancellation,
and the error path, with no dependencies beyond `HttpClient`.

### Binary resolution

1. `LLMUX_BINARY`
2. bundled `bin/llmux` next to the assembly (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

```sh
go build -o sdks/dotnet/bin/llmux ./cmd/llmux     # or: make sdk-bins
```

---

## Direct — `Llmux.LlmuxDirect`

llmux inside your process, through the C ABI documented in
[`ffi/README.md`](../../ffi/README.md). Read that first; this covers only what
is specific to .NET.

The C ABI is seven functions — `llmux_abi_version`, `llmux_new`, `llmux_close`,
`llmux_cancel`, `llmux_call`, `llmux_free`, `llmux_stream` — and `LlmuxDirect`
binds all seven. Six of them are unremarkable plumbing; `llmux_cancel` is the
one with a section of its own below, because it is the only way to get out of
a blocked `Call` or `StreamAsync` without destroying the whole gateway.

```csharp
using var llmux = LlmuxDirect.Open(configJson);

string models = llmux.Call("models");
string answer = llmux.Call("chat", requestJson);

await foreach (string chunk in llmux.StreamAsync(streamRequestJson, cancellationToken: ct))
{
    Console.Write(chunk);
}
```

JSON in, JSON out — the same JSON the HTTP API uses. The binding does not parse
it and pulls in no JSON dependency.

Worked example: [`examples/DirectChat.cs`](examples/DirectChat.cs).

### `LibraryImport`, and why the returns are `IntPtr`

The binding uses **`LibraryImport`** (the source-generated marshaller, .NET 7+)
rather than `DllImport`: the stubs are generated at compile time, they are
NativeAOT-compatible, and every string crossing the boundary has to be declared
rather than guessed.

That last point is the one that matters. **Every function returning a string
returns `IntPtr`, never `string`.** Declaring `llmux_call` as returning `string`
compiles, runs, and leaks: the runtime copies the C string into a managed one
and then has no idea that the original must go back to `llmux_free`, so it
either leaks it or — worse — frees it with the wrong allocator. The binding
therefore takes `IntPtr`, copies with `Marshal.PtrToStringUTF8`, and calls
`llmux_free` in a `finally`.

The `char** err` out-parameter is drained **even on the success path**, so a
library that sets a message alongside a success cannot leak it.

The library is located by a `NativeLibrary.SetDllImportResolver` hook, so
`LLMUX_LIBRARY` or an explicit path can point at a specific build without
touching `DllImportSearchPath`.

### `SafeHandle`

The gateway handle is a `SafeHandle` (`LlmuxDirect.LlmuxSafeHandle`), so:

- `using` closes it deterministically, on the exception path too;
- if you forget, the base class's finaliser still closes it, rather than never;
- `DangerousAddRef`/`DangerousRelease` around each call means a concurrent
  `Dispose` cannot close the handle mid-flight;
- double `Dispose` is safe, and calling after `Dispose` is a clean
  `LlmuxException`, not a crash.

llmux handles are `uint64` registry keys rather than pointers. `SafeHandle` is
still the right vehicle — it is the type the runtime knows how to keep alive
across a P/Invoke and release exactly once — and the key is stored in the
`IntPtr`, which is 64 bits on every platform llmux ships a library for.

### `StreamAsync` is bounded at one, and that is not a detail

`StreamAsync` is an `IAsyncEnumerable<string>` over the native callback. The
callback runs on a dedicated `LongRunning` thread (`llmux_stream` blocks for the
whole stream, so it must not sit on a thread-pool worker) and hands each chunk
to the consumer through a **`Channel` bounded at capacity 1**.

**Measured**, breaking out of `await foreach` after two chunks of a five-chunk
stream:

| channel | chunks consumed | times the native callback actually fired |
|---|---|---|
| `CreateUnbounded` | 2 | **5 — the whole completion ran** |
| `CreateBounded(1)` | 2 | **3 — two consumed, one in flight** |

With an unbounded channel the producer races ahead and the entire completion is
generated, streamed and metered before the consumer ever stops reading; `break`
then discards three chunks that already cost money. The two versions are
indistinguishable from outside the enumerable — only a counter inside the
callback tells them apart. Hence capacity 1.

**A plain `break` behaves as measured above: expect one chunk beyond your last
consumed one**, because the native callback only learns to stop the next time
it is invoked, and capacity 1 lets the producer run one chunk ahead.
**Cancelling the `CancellationToken` does not have that lag**, and that
distinction is the reason `StreamAsync` takes a token at all rather than
leaving cancellation to `break` plus `Dispose`. See the next section.

### Cancellation reaches `llmux_cancel`, not just the channel

Cancelling `StreamAsync`'s token does not merely stop feeding the channel —
`StreamAsync` registers a callback on the token that calls `Cancel()`
immediately, on whatever thread cancelled it. That matters because
`llmux_stream` spends most of a stream blocked in a network read on its own
`LongRunning` thread, not sitting inside the chunk callback where a channel
check could reach it. A bounded channel alone can only refuse the *next*
chunk once it arrives; it cannot interrupt a read that is already in flight
waiting for the one after that. `Cancel()` can, because it calls
`llmux_cancel`, which aborts the blocked native call directly.

**Measured**, against `sdks/fake-upstream.py` — ten words at 100 ms per
chunk, cancelled after the third delivered chunk:

```
$ dotnet --version
10.0.302
$ sdks/dotnet/run-examples.sh direct
...
cancellation (measured): consumer saw 3 chunk(s); upstream reports {"generated": 3, "streams": 1, "disconnects": 0}
```

The consumer stopped at 3; the upstream generated **3 of 12** (ten word
chunks plus a finish-reason frame plus a usage frame — the full count a run
to completion produces). Without `llmux_cancel` reaching the blocked call,
those other 9 frames would still have been generated, streamed over the
loopback socket, and metered, on the strength of a cancellation the consumer
believed had already taken effect.

An already-cancelled token throws `OperationCanceledException` before
`StreamAsync` does any native work at all — no gateway call is made on a
token that was never going to let it run.

**The exception you catch is `OperationCanceledException`, never the string
`"context canceled"`.** That string is what the C ABI actually returns
(`llmux_stream` fails with `*err` set to it); a .NET consumer should not have
to catch a generic `LlmuxException` and string-match its `Message` to
recognize its own cancellation. `StreamAsync` relies on
`ChannelReader.ReadAllAsync(cancellationToken)` observing the same token to
turn that into the exception type .NET consumers actually catch, and the
pump thread's own `LlmuxException` (carrying that string) is swallowed once
the stream has been told to stop — see the `catch (LlmuxException) when
(state.Stopped)` in `StreamAsync`.

**`Cancel()` — and therefore the token — is per-HANDLE, not per-stream.** It
aborts *every* call in flight on that gateway, including another
`StreamAsync` or `Call` you did not mean to touch. If your application needs
independent cancellation scopes for concurrent streams, open one
`LlmuxDirect` gateway per scope; there is no cheaper way to isolate them; a
gateway is inert until called, so this is not the resource cost it sounds
like (see "Latency is not the reason to embed" below).

`Cancel()` is also exposed directly, for callers not consuming a stream at
all: a stuck `Call`, or a callback-only host with no async iterator. It is
documented in code as: aborts every call in flight on this handle, leaves the
handle open and usable, safe from another thread and from inside a chunk
callback, and a no-op when nothing is running, when called twice, or after
the handle is already closed.

### The rule about exceptions

`OnChunk` is `[UnmanagedCallersOnly]`. **An exception crossing that boundary
terminates the process** — the same rule as a JNI or FFM upcall. Every path in
the callback is wrapped, and a failure becomes "stop the stream", which
`llmux_stream` unwinds cleanly; the exception is then rethrown from the
enumerable on the managed side. Write your consumer normally.

The project sets `AllowUnsafeBlocks` because the callback is passed as a
`delegate* unmanaged<IntPtr, IntPtr, int>` function pointer — the AOT-friendly,
allocation-free shape. `Llmux.cs`, the sidecar half, contains no unsafe code.

---

## The costs of the direct path

Not a footnote. Full detail in [`ffi/README.md`](../../ffi/README.md).

1. **The Go runtime lives in your process** — GC, scheduler, and signal
   handlers. It installs handlers for `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPIPE`
   and `SIGURG`, chaining to whatever was there. CoreCLR also uses `SIGSEGV`
   (null-check elimination, GC write barriers on some configurations) and
   `SIGRTMIN`-range signals for GC suspension on Linux. **This has been measured
   in detail for the JVM — see
   [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers) —
   and NOT for CoreCLR.** The Java findings are suggestive, not transferable:
   CoreCLR's handler set and its own chaining behaviour are different code. The
   .NET examples here ran clean, repeatedly, on darwin/arm64, which is evidence
   and not a proof. If you adopt the direct path on .NET, test it under load on
   your platform, and consider `DOTNET_gcServer`/profiler interactions
   explicitly.
2. **Not fork-safe.** After `fork()` without `exec()` the Go runtime in the
   child is broken. .NET gets off lightly: there is no `fork`-based worker model
   in the runtime, and `Process.Start` uses `fork`+`exec`. The concrete victims
   are a P/Invoke to bare `fork(2)`, and any supervisor that forks the process
   after the library has loaded.
3. **12–17 MB of shared library** — 12,823,104 bytes on darwin/arm64.
4. **Platform coverage is the dealbreaker for .NET.** See below.
5. **Latency is not the reason to embed.** ~4 µs in-process versus ~46 µs over
   loopback for the boundary itself; ~80–92 µs versus ~102–109 µs for a real
   chat call. Against a model taking hundreds of milliseconds that is noise. The
   reasons are: no second process, no port, no loopback surface, and a library
   that is inert until you call it — `llmux_new` starts no goroutines and opens
   no sockets, where the sidecar begins syncing its price catalog on startup.

## Platforms (direct path only)

| target | status |
|---|---|
| darwin/arm64 | built and smoke-tested. 12,823,104 bytes. The examples here were run on it. |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,356,264 bytes. |
| linux/amd64 | built and tested in CI only. |
| **windows/amd64** | **not built. No `llmux.dll` exists. Nobody has produced one.** |
| **darwin/amd64** | **not built.** |

**There is no Windows DLL**, so this file gives no Windows install instructions
for the direct path — there is nothing to install. `LlmuxDirect.FindLibrary()`
says so in its error message rather than failing with a bare
`DllNotFoundException`. On Windows, use the sidecar.

The **sidecar** has no such matrix.

## Toolchain this was built and run on

- .NET SDK **10.0.302**, targeting **net8.0**
- Go **1.25.12**, llmux **0.1.5** (the release `llmux_cancel` shipped in)
- darwin/arm64

The examples project sets `RollForward=LatestMajor` so a net8.0 build starts on
a machine that only has the .NET 10 runtime, which is the case here.

## Provider keys

Inherited from the environment (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
`GEMINI_API_KEY`, …) on both paths.

## Layout

```
sdks/dotnet/
  Llmux.cs                     sidecar (no unsafe, no native)
  LlmuxDirect.cs               direct, LibraryImport + SafeHandle + IAsyncEnumerable
  Llmux.csproj
  examples/DirectChat.cs       runnable
  examples/SidecarChat.cs      runnable
  examples/Program.cs          picks one
  examples/Examples.csproj
  run-examples.sh              builds everything, runs both
  tests/SidecarTests.cs        the sidecar test suite
  tests/DirectCancelTests.cs   llmux_cancel / StreamAsync cancellation, gated on python3 + libllmux
```

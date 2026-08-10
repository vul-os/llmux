# llmux language packages

Use llmux from any of fifteen languages, **two ways**:

- **Direct** — in-process. Go imports the package. Every other language loads a
  C ABI shared library of seven symbols — `llmux_new`, `llmux_call`,
  `llmux_stream`, `llmux_cancel`, `llmux_close`, `llmux_free` and
  `llmux_abi_version` — built with `go build -buildmode=c-shared`. See
  [`../ffi/README.md`](../ffi/README.md) and
  [the C ABI](../docs/c-abi.md).
- **Sidecar** — the gateway as a separate process, either one you run or one the
  package spawns and manages for you on `127.0.0.1`.

Neither is the right answer everywhere. **The "Default" column is a real
recommendation, not a formality** — for several languages the honest advice is
the sidecar, and the reason is in that language's README.

| Language | Direct | Sidecar | Default | Streaming (direct) | Cancelling (direct) |
|---|---|---|---|---|---|
| [go](go/) | package import — **no FFI, no shared library** | ✓ | **direct** | `ChunkFunc` callback | `context.Context` — **per call, not per gateway** |
| [c](c/) | ✓ links `libllmux` | ✓ | **direct** | C callback | `llmux_cancel` from a `pthread`, or from the callback |
| [cpp](cpp/) | ✓ header-only RAII (`llmux.hpp`) | ✓ | **direct** | C callback | `std::stop_token` (C++20), or `cancel()` |
| [rust](rust/) | ✓ `libloading` | ✓ | direct | iterator | dropping the iterator; `CancelHandle` (`Send + Sync`) |
| [swift](swift/) | ✓ SwiftPM, C interop | ✓ | direct | `AsyncSequence` | `Task` cancellation, via `onTermination` |
| [deno](deno/) | ✓ `Deno.dlopen` | ✓ | direct | `for await` | `AbortSignal`, and `break` |
| [bun](bun/) | ✓ `bun:ffi` | ✓ | direct | `for await` (worker-backed) | `AbortSignal` — **written but unrun, no Bun here** |
| [node](node/) | ✓ koffi | ✓ | **sidecar** for servers | callback only — see below | `AbortSignal`, but only from inside the callback |
| [python](python/) | ✓ `ctypes` | ✓ | **sidecar** | callback + `stream_iter` | abandoning the generator; `cancel()` from a thread |
| [java](java/) | ✓ FFM (JDK 22+) | ✓ | **sidecar** | callback | `Thread.interrupt` / `Future.cancel(true)`; `cancel()` |
| [kotlin](kotlin/) | ✓ over the Java binding | ✓ | **sidecar** | `Flow` | cancelling the coroutine, incl. `withTimeout` |
| [dotnet](dotnet/) | ✓ `LibraryImport` + `SafeHandle` | ✓ | **sidecar** | `IAsyncEnumerable` | `CancellationToken` |
| [ruby](ruby/) | ✓ `fiddle` (stdlib) | ✓ | depends — see README | callback | `stream_enum` + `break`; `cancel` from another thread |
| [php](php/) | ✓ `FFI` extension | ✓ | **sidecar** | callback | `streamGenerator` + `break` (Fiber-backed); `cancel()` |
| [elixir](elixir/) | **none, deliberately** | ✓ | **sidecar** | n/a | n/a |

**`llmux_cancel` is per HANDLE, not per call.** It aborts every call in flight on
that gateway, so the per-stream shapes above — an `AbortSignal`, a
`CancellationToken`, a cancelled `Task` — are per-stream only as long as one
gateway is running one stream. If two streams share a gateway, cancelling either
ends both. One gateway per cancellation scope is the fix. Go is the exception,
and the only one: it carries a `context.Context` per call and needs no symbol.

## Why the sidecar is the default in seven of them

Three of the reasons below are measured runtime hazards, and the numbers are in
the language READMEs. Two are not measurements and are marked as such: .NET's
reason is platform coverage, and Elixir's is what a NIF is.

- **Java / Kotlin.** Loading the library replaces five of HotSpot's signal
  handlers (`SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPIPE`, `SIGURG`) and adds
  `SA_ONSTACK` to three more, including `SIGUSR2`, which HotSpot uses to suspend
  threads. Both runtimes still work — two million recovered implicit NPEs say so
  — and `libjsig` fixes it cleanly. But `libjsig` is a flag on the **java launch
  command**, and a library cannot add one to a process that already started.
  (`SIGPROF` is *not* touched: JFR profiling is unaffected. The commonly-cited
  hazard does not exist here — see [`java/signal-probe.sh`](java/signal-probe.sh).)
- **Python / PHP** — and **Ruby**, which is the "depends" row for this same
  reason. The Go runtime is **not fork-safe**. Measured in real php-fpm: a
  worker that loaded the library in the master answers `models` in 0.1 ms and
  then never answers `chat` at all. Same shape after `os.fork()` in Python. Note
  the trap — **`models` succeeds in a broken child**, so a health check that only
  lists models is a false green for a process that will hang on the first real
  request. Python's fix is the `spawn` start method; PHP-FPM and Unicorn fork by
  design. Ruby is a "depends" rather than a "sidecar" because whether it forks is
  a deployment choice: Unicorn, Passenger and clustered Puma do, so use the
  sidecar; single-mode Puma, Falcon, Sidekiq and CLI tools do not, and direct is
  fine there. See [`ruby/README.md`](ruby/README.md).
- **Node.** A Node thread that has entered a Go `c-shared` library never
  terminates, so neither `worker_threads` nor koffi's async pool can move
  streaming off the main thread — the process hangs at exit. Node direct mode is
  therefore synchronous and takes a callback rather than an async iterator.
  Buffering the whole answer and replaying it as fake chunks would be worse than
  an honest HTTP call.
- **.NET.** Not a runtime hazard but a coverage one: **there is no Windows
  shared library** — not "untested", not built by anyone, ever. .NET has a large
  Windows install base, so for much of it the direct path does not exist. The
  signal findings above were measured on the JVM and explicitly *not* on CoreCLR,
  so they are suggestive rather than transferable.
- **Elixir.** In-process would mean a NIF: it cannot be killed or
  `Task.await`-timed-out, a segfault takes the whole VM, and a dirty-IO NIF caps
  concurrency at the scheduler count. Every safe alternative reintroduces the
  second process the C ABI exists to remove.

## Costs that apply to every direct binding

- **The Go runtime lives in your process** — its GC, scheduler and signal
  handlers.
- **Not fork-safe.** See above.
- **`dlclose` hangs.** Load the library once per process and leave it mapped;
  every binding here does.
- **Cancelling a stream now stops the upstream call — measure it anyway.** This
  used to be the standing warning here: a buffered async wrapper let the callback
  run ahead, and a consumer taking 3 of 10 chunks still caused every chunk after
  those 3 to be generated **and metered**, invisibly, because from the consumer's
  side the two outcomes look identical. `llmux_cancel` (0.1.5) is the fix, and
  every direct binding now reaches it from that language's own cancellation
  construct. The proof is a fake upstream that counts what it actually generated
  and serves the count at `GET /generated`, so the claim is checked from the
  provider's side rather than the consumer's: **a consumer stopping at 3 chunks
  leaves the upstream at 3, against 12 for a run to completion.** Each README
  carries its own measured pair, and where a language cannot reach the symbol in
  time — Node's blocked event loop, Java's per-chunk interrupt poll — it says so
  and gives the worse number instead of the good one.
- **Latency is not the reason to embed.** The boundary is ~4 µs in-process
  versus ~46 µs over loopback, but a real chat call measures ~80–92 µs against
  ~102–109 µs — noise next to a model answering in hundreds of milliseconds. The
  reasons are: no second process, no port, no loopback surface.

## Shared libraries — what actually exists

Direct mode needs a shared library for your platform, and **no release ships
one.** You build it with [`../scripts/build-ffi.sh`](../scripts/build-ffi.sh),
or `make ffi` for this host, which writes it to `dist/ffi/<goos>_<goarch>/`.
Where that build is known to work today:

| Target | Status |
|---|---|
| darwin/arm64 | built and smoke-tested |
| linux/arm64 | built and smoke-tested |
| linux/amd64 | CI only |
| **windows/amd64** | **does not exist — no DLL ships** |
| **darwin/amd64** | **not built** |

"Built and smoke-tested" means the maintainer compiled and ran it on that
target, not that you can download it. **openrate's matrix is different** (no
linux/arm64; an unexecuted darwin/amd64 build) — do not assume one covers the
other.

The sidecar path has none of these constraints — it needs only the `llmux`
binary for your platform.

## The measurement harness

[`fake-upstream.py`](fake-upstream.py) is an OpenAI-compatible upstream that
sleeps a configurable amount before each chunk, **counts the chunks it actually
wrote to a socket**, and serves that count at `GET /generated`. It stops
generating the moment the client's connection goes away — without that check it
would keep counting into a dead socket and every language would report the full
run, which is the bug being measured, arrived at by accident.

```
python3 sdks/fake-upstream.py --chunk-delay-ms 100 --text "one two three four five six seven eight nine ten"
```

It prints the same `URL` / `CONFIG` / `TEXT` lines `ffi/fakeupstream` prints, so
a runner that parses one parses the other. `ffi/fakeupstream` — the Go fake the
C smoke test and the benchmark drive — is still what the ordinary examples use;
this one exists for the one question that fake cannot answer. The three
JavaScript runtimes answer the same contract from their own
`examples/fake-upstream.mjs`, kept byte-identical across node, deno and bun, so
that running a Node example does not require a Python on the box.

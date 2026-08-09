# llmux language packages

Use llmux from any of fifteen languages, **two ways**:

- **Direct** — in-process. Go imports the package. Every other language loads a
  C ABI shared library of six symbols — `llmux_new`, `llmux_call`,
  `llmux_stream`, `llmux_close`, `llmux_free` and `llmux_abi_version` — built
  with `go build -buildmode=c-shared`. See
  [`../ffi/README.md`](../ffi/README.md) and
  [the C ABI](../docs/c-abi.md).
- **Sidecar** — the gateway as a separate process, either one you run or one the
  package spawns and manages for you on `127.0.0.1`.

Neither is the right answer everywhere. **The "Default" column is a real
recommendation, not a formality** — for several languages the honest advice is
the sidecar, and the reason is in that language's README.

| Language | Direct | Sidecar | Default | Streaming (direct) |
|---|---|---|---|---|
| [go](go/) | package import — **no FFI, no shared library** | ✓ | **direct** | `ChunkFunc` callback |
| [c](c/) | ✓ links `libllmux` | ✓ | **direct** | C callback |
| [cpp](cpp/) | ✓ header-only RAII (`llmux.hpp`) | ✓ | **direct** | C callback |
| [rust](rust/) | ✓ `libloading` | ✓ | direct | iterator |
| [swift](swift/) | ✓ SwiftPM, C interop | ✓ | direct | `AsyncSequence` |
| [deno](deno/) | ✓ `Deno.dlopen` | ✓ | direct | `for await` |
| [bun](bun/) | ✓ `bun:ffi` | ✓ | direct | `for await` (worker-backed) |
| [node](node/) | ✓ koffi | ✓ | **sidecar** for servers | callback only — see below |
| [python](python/) | ✓ `ctypes` | ✓ | **sidecar** | callback + `stream_iter` |
| [java](java/) | ✓ FFM (JDK 22+) | ✓ | **sidecar** | callback |
| [kotlin](kotlin/) | ✓ over the Java binding | ✓ | **sidecar** | `Flow` |
| [dotnet](dotnet/) | ✓ `LibraryImport` + `SafeHandle` | ✓ | **sidecar** | `IAsyncEnumerable` |
| [ruby](ruby/) | ✓ `fiddle` (stdlib) | ✓ | depends — see README | callback |
| [php](php/) | ✓ `FFI` extension | ✓ | **sidecar** | callback |
| [elixir](elixir/) | **none, deliberately** | ✓ | **sidecar** | n/a |

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
- **Cancelling a stream does not always stop the upstream call.** A buffered
  async wrapper can let the callback run ahead — in one measured case a consumer
  taking 3 of 10 chunks still caused all 10 to be generated **and metered**.
  Each README states its measured callback count under early exit.
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

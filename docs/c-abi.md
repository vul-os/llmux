# The C ABI

Go hosts import [`core/gateway`](embedding.md). Every other language either
loads the shared library described here and runs llmux **inside its own
process**, or talks to the `llmux serve` sidecar over HTTP.

This page is the reader-facing contract. The implementation notes, the
directory layout and the test strategy live in
[`ffi/README.md`](../ffi/README.md); the header itself,
[`ffi/include/llmux.h`](../ffi/include/llmux.h), is the normative surface.

**Read [the costs](#the-costs) before you commit to this.** For several hosts
the sidecar is the better answer, and saying so is part of the job.

## Seven functions

```c
const char* llmux_abi_version(void);

uint64_t llmux_new(const char* config_json, char** err);
void     llmux_close(uint64_t h);
void     llmux_cancel(uint64_t h);

char*    llmux_call(uint64_t h, const char* method, const char* request_json, char** err);
void     llmux_free(char* p);

typedef int (*llmux_chunk_cb)(const char* chunk_json, void* user_data); /* 0 = continue */
int llmux_stream(uint64_t h, const char* method, const char* request_json,
                 llmux_chunk_cb cb, void* user_data, char** err);
```

`llmux_cancel` is new in 0.1.5 and is the seventh symbol.

That is the whole surface. `go build -buildmode=c-shared` also emits a
`libllmux.h` next to the library, but it drags in Go's typedefs and drops the
`const` qualifiers — use the hand-written header.

**Requests and responses are JSON — the same JSON the HTTP API uses.** A body
that works against `POST /v1/chat/completions` works here unchanged, and what
comes back is what that endpoint returns. That reuses a wire contract which is
already tested and documented, and keeps the surface small enough to bind from a
dozen languages.

`method` is a string rather than one C function per operation, so the header
stays stable as llmux grows methods.

| function | `method` | request | result |
|---|---|---|---|
| `llmux_call` | `"chat"` | OpenAI chat-completions body | a chat completion object |
| `llmux_call` | `"embed"` | OpenAI embeddings body | an embeddings response |
| `llmux_call` | `"models"` | ignored — pass `NULL` | an OpenAI model list |
| `llmux_stream` | `"chat"` | OpenAI chat-completions body | one chunk per callback |
| `llmux_cancel` | — | — | aborts everything in flight on the handle, without closing it |

A `"chat"` request with `"stream": true` is **refused** by `llmux_call` rather
than quietly served as one blob after the fact. Use `llmux_stream`.

## The rules

**Ownership.** Every non-const `char*` this library returns — results *and*
error messages — is freed with `llmux_free`, and with nothing else.
`llmux_abi_version` is the single exception: it returns **the same pointer on
every call**, allocated once when the library loads and never freed. You must
not free it, and freeing it corrupts the allocator for everything else in your
process. `llmux_free(NULL)` is safe.

**Errors.** Fallible functions take a trailing `char** err`. **On entry they set
`*err` to `NULL`**; on failure they write a malloc'd, human-readable UTF-8
message there. So `*err != NULL` after a call always means *that* call failed,
and one `char *err = NULL;` is safe to reuse across calls. Before 0.1.5 `*err`
was only ever written on failure, so a success left the previous failure's
message in place and a binding that reused the variable freed it twice, in the
host's allocator. Clearing does not free what was there — ownership passed to
you when it was written, so free it before you reuse the variable. Pass `NULL`
if you do not want the message. It is **not** JSON — do not parse it.

**Handles are integers in a registry inside the library, never pointers**, and
they are never reused. Calling with a closed or invented handle is a clean error
string, not a segfault in your process. `llmux_close` is idempotent.

**Panics never reach you.** A Go panic anywhere inside the library — including
one thrown by *your own chunk callback*, which runs inside the stream call frame
— is recovered at the entry point and returned as an ordinary error with its
stack in the message. This matters because in `c-shared` mode a panic does not
unwind into C: an escaping one is a Go runtime fatal error that ends your
process. The HTTP shell has had this backstop since before v0.1.0, so until
0.1.5 the same bug was a logged 500 as a sidecar and a dead uWSGI or JVM worker
as a library — the two modes this documentation presents as interchangeable.

**`llmux_stream` requires a non-NULL callback.** The latitude `err` gets does not
extend to `cb`: a NULL callback returns `-1` and sets `*err` to
`llmux: llmux_stream requires a non-NULL chunk callback`, because a stream with
nowhere to put its chunks is a bug rather than a way to discard them.

**Threading.** A handle is safe to use from several threads at once.

**`llmux_cancel` is how you abandon a blocked call.** `llmux_call` and
`llmux_stream` are synchronous and block until they finish; calling
`llmux_cancel(h)` from another thread aborts everything in flight on that
handle and **leaves the handle open** — the next call starts on a fresh context.
Cancelling an unknown handle, or one with nothing running, is a no-op. A
cancelled stream that had already delivered chunks returns `-1` with `*err` set,
and tokens already served are still metered.

Before 0.1.5 the only escape from a blocked call was `llmux_close`, which
destroys the gateway *and every other stream on it*. That is why `llmux_cancel`
exists: "I have lost interest in this request" and "tear the whole instance
down" were the same button.

`llmux_cancel` is also not a substitute for the liveness bounds llmux applies to
a stream on its own (see [Streaming](#streaming)). Those are for when the
*upstream* goes quiet; this is for when *your* side loses interest.

**`llmux_close` drains, so it can block.** It cancels the calls in flight and
then **waits up to a 5 s grace** for them to return before releasing the Redis
client and Postgres pool — releasing those under a running call would be a
use-after-close inside your process. Closing also aborts any `llmux_stream`
still running on the handle from another thread; that call returns as if the
consumer had stopped it.

> **`llmux_close` must not be called from inside a chunk callback.** That would
> be waiting on the very call that is running it. Return from your callback
> first — return non-zero to stop the stream — and close afterwards. The 5 s
> bound is deliberate rather than generous: `llmux_close` is `void`, so it has
> no way to report a failure, and an unbounded drain would deadlock exactly that
> case instead of merely being slow.

Closing an unknown or already-closed handle is still a no-op, so cleanup paths
can be idempotent.

**No authentication on this boundary, by design.** Virtual keys, budgets and
per-key model allow-lists are enforced by the HTTP shell's auth middleware. An
in-process host is already inside the trust boundary and calls the gateway
directly, the same as a Go embedder does — except that a Go embedder can call
`gw.Authorize` and you cannot, because it is not on this ABI. **If you need
per-tenant keys and budgets enforced at the boundary, that is the sidecar's
job.**

## Construction is inert — with the same three exceptions as Go

`llmux_new` starts no goroutines and opens no sockets. There is no background
price-catalog sync and no spend flusher in library mode: a shared library loaded
into someone else's process must not start background traffic they did not ask
for. If you want that, run the sidecar.

`config_json` is an llmux configuration document — the same JSON `llmux serve`
reads from `llmux.json`. `NULL` or `""` means built-in defaults plus the
`LLMUX_`-namespaced environment (auto-detected providers, `LLMUX_*` overrides).
It goes through `config.FromJSON`:

- **A configuration naming a Postgres DSN connects and migrates eagerly** during
  `llmux_new`. That is the one thing that makes construction non-inert, and it
  is an explicit opt-in you wrote into the config you passed. The connect is
  bounded by `postgres_connect_timeout_seconds` (30 s by default) rather than
  able to park your thread forever against a black-holed DSN.
- **Any provider configured with `api_key_env` is read from the environment**
  during `llmux_new`, because that is what the field means. If your host process
  scrubs or rewrites its environment before loading the library, the credential
  is not there to read.
- **`LLMUX_*` environment overrides apply** whether or not you passed a config
  document — a `NULL` config is "defaults plus environment", not "nothing".

### Two precedence rules changed in 0.1.5, and both are breaking

Both correct the same mistake: a library reading its host's environment.

**Your document wins over the environment.** A field your document states is
never overwritten by an environment variable — including one you set to `""` or
`0`, which is a statement and not a gap. Variables fill in only what the
document is silent about. `applyEnv` used to run *after* the merge and override
it unconditionally. The sidecar (`config.Load`) resolves the other way round and
is unchanged: there the operator owns the file *and* the environment, whereas
here the environment belongs to your application and llmux is a guest in it.

**`DATABASE_URL` and `VULOS_DATABASE_URL` are ignored in library mode.** They
are your application's variable names, not llmux's. Since `postgres` is the one
field that turns an inert construction into remote I/O, adopting a DSN llmux was
never handed meant **migrating your production database because you loaded a
shared library**: a Rails or Django app with `DATABASE_URL` exported got
`CREATE SCHEMA` and `CREATE TABLE` run in it, while the header promised
inertness "unless *your configuration* names a Postgres DSN". `LLMUX_POSTGRES`
is namespaced, unambiguous and still honoured; to use a database from a host
that sets `DATABASE_URL`, put the DSN in your document.

Full table: [configuration](configuration.md#configuration-precedence-depends-on-who-is-asking).

## Streaming

`llmux_stream` blocks until the stream ends and calls `cb` once per chunk. Each
`chunk_json` is one `chat.completion.chunk` object — byte for byte what the HTTP
API writes after `data: ` in its SSE stream, without the prefix and without the
terminal `[DONE]` frame. **The string is owned by the library and valid only for
the duration of the callback; copy it if you need it afterwards.**

Returns `0` on success, `-1` on failure with `*err` set. A callback that returns
non-zero **stops the stream and is not a failure**: the return is `0` and `*err`
is left `NULL`. You returned non-zero, so you already know it happened. Tokens
already served are metered either way.

**A stream has liveness bounds, not a deadline** (new in 0.1.5).
`stream_first_byte_timeout_seconds` (default **60**) bounds the wait for the
first chunk; `stream_idle_timeout_seconds` (default **120**) bounds the gap
between chunks and is **re-armed on every chunk**, so a generation that runs for
an hour is never truncated while a connection that went quiet is caught in
seconds. A negative value disables either; `0` selects the default. When a bound
fires, `llmux_stream` returns `-1` with `*err` set to `llmux: streaming upstream
stopped responding`.

There is deliberately no wall-clock deadline, and that is why there used to be
no bound at all: a total timeout cannot distinguish a long generation from a
dead connection, so it would have to truncate the correct case to catch the
broken one. Time-to-first-chunk and inter-chunk gap can tell them apart. Before
0.1.5 a stream against an upstream that accepted the connection and then said
nothing blocked its caller forever — and in library mode that caller is one of
**your** threads.

These bounds are about the upstream going quiet. When *you* want out, call
[`llmux_cancel`](#the-rules) from another thread.

**Stopping the consumer is not the same as stopping the stream.** This ABI stops
promptly — the next chunk after your non-zero return is not requested. What does
not stop promptly is a *buffered* wrapper built on top of it in your language: if
the callback pushes into a queue that an async iterator drains, the callback runs
ahead of the consumer and keeps returning zero after the consumer has walked
away. In one measured case a consumer that took **3 of 10 chunks still caused all
10 to be generated, and metered**. If you are writing a binding, make the
consumer's exit visible to the callback and state the measured callback count
under early exit in your README, the way the packages in
[`sdks/`](https://github.com/vul-os/llmux/tree/main/sdks) do. If you are choosing
one, read that number rather than assuming `break` saves money.

**Which thread the callback runs on.** It runs on the thread that called
`llmux_stream`, synchronously, before `llmux_stream` returns.
[`ffi/ctest/smoke.c`](../ffi/ctest/smoke.c) asserts this by comparing
`pthread_self()` inside the callback against the caller's, on every CI run,
because a claim about threads that nobody measures is how bindings get written
against a fiction.

It is still a callback into your host from inside a Go call frame:

- **Python** must reacquire the GIL in the callback. `ctypes.CFUNCTYPE` does
  this for you; `PYFUNCTYPE` does not.
- **JVM** hosts must have the thread attached to the VM.
- If your language's FFI cannot safely accept a C callback at all, **use the
  sidecar's SSE stream for streaming instead of pretending.** A binding that
  buffers the whole answer and replays it as fake chunks is worse than an honest
  HTTP call, because it looks right until someone measures time to first token.

## A minimal C host

```c
#include <stdio.h>
#include "llmux.h"

int on_chunk(const char* chunk_json, void* ud) {
    (void)ud;
    fputs(chunk_json, stdout);   /* valid only inside this call — copy to keep */
    return 0;                    /* non-zero would stop the stream */
}

int main(void) {
    printf("llmux %s\n", llmux_abi_version());   /* static: do NOT free */

    char* err = NULL;
    uint64_t h = llmux_new(NULL, &err);          /* defaults + environment */
    if (!h) { fprintf(stderr, "%s\n", err); llmux_free(err); return 1; }

    const char* req = "{\"model\":\"gpt-4o-mini\","
                      "\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}";

    char* out = llmux_call(h, "chat", req, &err);
    if (!out) { fprintf(stderr, "%s\n", err); llmux_free(err); llmux_close(h); return 1; }
    puts(out);
    llmux_free(out);                             /* llmux_free, never free() */

    if (llmux_stream(h, "chat", req, on_chunk, NULL, &err) != 0) {
        fprintf(stderr, "%s\n", err);
        llmux_free(err);
    }

    llmux_close(h);                              /* idempotent */
    return 0;
}
```

Check `llmux_abi_version()` against the version your bindings were generated
for, at startup. A shared library is resolved off a load path you may not
control; without that probe, a stale `libllmux` earlier on the path is called
silently and misbehaves in ways that look like llmux bugs.

## Thirteen bindings already exist

Before you bind these seven functions by hand, check whether your language is
already done. Thirteen of the fifteen packages under
[`sdks/`](https://github.com/vul-os/llmux/tree/main/sdks) load this exact
library, each through its own language's FFI:

| Language | How it loads `libllmux` | Streaming across the boundary |
|---|---|---|
| [C](https://github.com/vul-os/llmux/tree/main/sdks/c) | links it | C callback |
| [C++](https://github.com/vul-os/llmux/tree/main/sdks/cpp) | header-only RAII wrapper (`llmux.hpp`) | C callback |
| [Rust](https://github.com/vul-os/llmux/tree/main/sdks/rust) | `libloading` | iterator |
| [Swift](https://github.com/vul-os/llmux/tree/main/sdks/swift) | SwiftPM C interop | `AsyncSequence` |
| [Deno](https://github.com/vul-os/llmux/tree/main/sdks/deno) | `Deno.dlopen` | `for await` |
| [Bun](https://github.com/vul-os/llmux/tree/main/sdks/bun) | `bun:ffi` | `for await`, worker-backed |
| [Node](https://github.com/vul-os/llmux/tree/main/sdks/node) | koffi | **callback only** — a Node thread that enters the library never terminates |
| [Python](https://github.com/vul-os/llmux/tree/main/sdks/python) | `ctypes` | callback, plus `stream_iter` |
| [Java](https://github.com/vul-os/llmux/tree/main/sdks/java) | FFM (JDK 22+) | callback |
| [Kotlin](https://github.com/vul-os/llmux/tree/main/sdks/kotlin) | over the Java binding | `Flow` |
| [.NET](https://github.com/vul-os/llmux/tree/main/sdks/dotnet) | `LibraryImport` + `SafeHandle` | `IAsyncEnumerable` |
| [Ruby](https://github.com/vul-os/llmux/tree/main/sdks/ruby) | `fiddle` (stdlib) | callback |
| [PHP](https://github.com/vul-os/llmux/tree/main/sdks/php) | the `FFI` extension | callback |

The two that are not on that list are deliberate, not missing.
[Go](https://github.com/vul-os/llmux/tree/main/sdks/go) imports
[`core/gateway`](embedding.md) and never touches a C boundary at all.
[Elixir](https://github.com/vul-os/llmux/tree/main/sdks/elixir) has **no direct
mode on purpose**: in-process would mean a NIF, which cannot be killed or
`Task.await`-timed-out, takes the whole VM down on a segfault, and — as a
dirty-IO NIF — caps concurrency at the scheduler count.

Every one of those thirteen also keeps a sidecar path, because the shared
library does not exist on every platform they run on. **Seven of the fifteen
recommend the sidecar by default** and one more says it depends on your
deployment, so being on this table is not the same as being told to use it.
The reasons are measured, per language, in
[`sdks/README.md`](https://github.com/vul-os/llmux/blob/main/sdks/README.md) and
summarised in [Language packages](sdks.md#which-mechanism-each-language-defaults-to).

## The costs

`-buildmode=c-shared` is not free, and none of this belongs in a footnote.

1. **The Go runtime lives in your process** — its garbage collector, its
   scheduler, and its signal handlers. Measured on a JVM host, loading the
   library **replaces five handlers** (`SIGSEGV`, `SIGBUS`, `SIGFPE`,
   `SIGPIPE`, `SIGURG`) and adds `SA_ONSTACK` to three more, including
   `SIGUSR2`, which HotSpot uses to suspend threads. **`SIGPROF` is not
   touched**, so the profiler hazard everyone cites does not exist here and JFR
   keeps working — that is measured by
   [`sdks/java/signal-probe.sh`](https://github.com/vul-os/llmux/blob/main/sdks/java/signal-probe.sh),
   not assumed. Go chains to a pre-existing handler in most cases, but "most"
   is the honest word. On the JVM `libjsig` fixes it cleanly and is a flag on
   the **java launch command**, which a library cannot add to a process that
   has already started — hence the sidecar default for Java and Kotlin.

2. **It is not fork-safe.** After `fork()` without `exec()`, the Go runtime in
   the child is broken: its threads did not come across, so the first call into
   the library can hang or crash. This bites:
   - Python `multiprocessing` with the default `fork` start method on Linux —
     use `multiprocessing.set_start_method("spawn")`.
   - **uWSGI**, **Unicorn**, **php-fpm**, and any other pre-fork worker model —
     load the library *after* the fork, in the worker, never in the master.

   **Watch the false green.** Measured in real php-fpm, and again after
   `os.fork()` in Python: a broken child answers `models` in about 0.1 ms —
   that call is served from memory and never reaches the scheduler — and then
   never answers `chat` at all. A readiness probe that lists models reports a
   healthy worker that will hang on its first real request.

3. **Building it needs cgo and a C toolchain per target platform.** A consumer
   only needs the built artifact — but no release publishes one, so "somebody"
   is you or your build pipeline, per platform, and a cgo cross-compile needs a
   cross C compiler, not just a `GOOS` variable. See
   [building it yourself](#building-it-yourself).

4. **It is a big artifact.** Measured: ~12 MB on darwin/arm64, ~17 MB on
   linux/arm64. `-ldflags="-s -w"` takes the darwin build to ~11 MB.

5. **Two libraries, two runtimes.** If your process loads `libllmux` *and*
   another Go-built `c-shared` library, you get two independent Go runtimes with
   two GCs. It works; it is not free either.

**When the sidecar is the better answer.** If your host pre-forks, if it embeds
another runtime with strong opinions about signals, if you need per-tenant keys
and budgets enforced at the boundary, or if your FFI cannot take a callback —
run `llmux serve` and talk HTTP to it. Choosing the sidecar is a supported
outcome of reading this page, not a failure.

## Latency is not the reason

Measured, not asserted. [`ffi/bench`](../ffi/bench) dlopens the real shared
library and drives it from the same Go program that drives an `llmux serve` HTTP
handler over loopback, against the same fake upstream — so the difference is the
transport, not two HTTP clients written in different languages.

darwin/arm64 (Apple silicon laptop), 1000 measured requests each, Go 1.25.12,
llmux 0.1.2, median of three runs:

| workload | in-process (C ABI) | loopback HTTP | saved |
|---|---|---|---|
| `models` — answered from memory, no upstream | **~4.0 µs** | ~46–49 µs | ~43 µs |
| `chat` — includes the upstream round trip | ~80–92 µs | ~102–109 µs | ~10–28 µs |

The `models` row is the boundary itself — a cgo call plus JSON in and JSON out,
against a loopback TCP round trip plus HTTP framing. The `chat` row is that same
saving inside a request that actually does something.

**Against a real model this is noise.** A real completion takes hundreds of
milliseconds to tens of seconds; saving 40 µs of transport is a rounding error,
and anyone choosing in-process *for the latency* is optimising the wrong thing.
The loopback HTTP row is measured with keep-alive on, i.e. the sidecar at its
best. The reasons to be in-process are no second process to supervise, no port
to bind, no loopback surface to secure, and no request bodies crossing a socket.

## Where it runs

Honest status, so nobody reads a supported matrix into a loop that ran three
times:

| target | status |
|---|---|
| **darwin/arm64** | Built and tested on the development machine. 12,823,104 bytes; the C smoke test passes all 40 checks against it |
| **linux/arm64** | Built and tested in a `golang:1.25` container on that same machine. 17,356,264 bytes; all 40 checks pass |
| **linux/amd64** | Built and tested **in CI** (the `ffi` job on `ubuntu-latest`). Not produced on the development machine — no cross toolchain there |
| **windows/amd64** | **Not built. Not tested. No `.dll` has been produced by anyone yet.** `build-ffi.sh` will attempt it if `x86_64-w64-mingw32-gcc` or `zig` is present; nobody has run that, so treat Windows as unverified |
| **darwin/amd64** | **Not built** — no Intel macOS machine or SDK here |

If you ship to Windows or Intel macOS, there is no prebuilt library for you
today and the sidecar is the supported path for those targets.

## Building it yourself

```bash
scripts/build-ffi.sh              # this host only
scripts/build-ffi.sh --all        # plus every target with a real toolchain here
scripts/build-ffi.sh --out DIR    # default: dist/ffi
```

Output:

```text
dist/ffi/<goos>_<goarch>/libllmux.so | libllmux.dylib | llmux.dll
dist/ffi/<goos>_<goarch>/libllmux.h    (cgo-generated)
dist/ffi/include/llmux.h               (the stable hand-written header)
```

The script only attempts a target when it can find a C compiler that can
actually produce that target's objects, and it prints, by name and with a
reason, every target it skipped. `--all` on a machine with no cross toolchain
builds exactly one library and says so.

Getting a cross toolchain, if you want one:

- linux/amd64 from Debian/Ubuntu: `apt install gcc-x86-64-linux-gnu`
- windows/amd64: `apt install gcc-mingw-w64-x86-64`, or install `zig`, which
  `build-ffi.sh` will use as `zig cc -target x86_64-windows-gnu`
- The most reliable path to Linux artifacts from a Mac is a container, which is
  exactly what the linux/arm64 row above did.

## How it is tested

```bash
make test-ffi     # gated Go unit tests + the C smoke test
```

Two layers, and neither replaces the other:

- **`ffi/abi_test.go`** — 14 tests in pure Go, no cgo: handle registry,
  use-after-close, method dispatch, streaming, abort semantics, two independent
  gateways in one process, and the version constant against `VERSION`.
- **`ffi/ctest/smoke.c`** — dlopens the built library, resolves all seven symbols
  by name, and runs one unary call and one streaming call end to end. This is
  the layer that catches a missing `//export`, a renamed symbol, or a header
  that has drifted from the library — none of which any Go test would notice. It
  asserts 40 named checks **and then asserts that 40 checks ran**, because a C
  program that returns 0 having executed three of them looks identical to one
  that executed all of them.

Both run in CI through `scripts/go-test-gate.sh`, which fails a run in which
zero tests executed — the classic false green for a nested module.

`ffi/` is a **separate Go module** whose path is `github.com/vul-os/llmux-ffi`,
deliberately outside `github.com/vul-os/llmux/`. Both facts are load-bearing:
the separate module keeps cgo out of the repo-root `go build ./...` and the
`-tags noui` build, and the path outside the prefix means Go's `internal/` rule
applies to this code exactly as it does to any third-party embedder — so the C
ABI is *evidence* that llmux's exported API is sufficient, not a privileged
insider. `TestFFIUsesOnlyThePublicAPI` asserts both.

## Related

- [Choosing a mode](choosing-a-mode.md) — when this is the wrong tool
- [Language packages](sdks.md) — what already exists per language
- [Embedding llmux](embedding.md) — the Go-native version of this boundary
- [`ffi/README.md`](../ffi/README.md) — layout, build detail, test strategy
- [`ffi/include/llmux.h`](../ffi/include/llmux.h) — the normative header

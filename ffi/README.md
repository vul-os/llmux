# llmux as a C shared library

llmux is an OpenAI-compatible LLM gateway — routing, retries, failover,
sovereignty enforcement, BYOK, caching, pricing, metering. There are two ways to
use it, and both are supported:

- **Direct (in-process).** Go hosts import `github.com/vul-os/llmux/core/gateway`.
  Every other language loads the shared library this directory builds and runs
  llmux *inside its own process*.
- **Sidecar (out-of-process).** `llmux serve` over HTTP, run by the operator or
  spawned and managed by one of the SDKs in `sdks/`.

This document is about the first one. Read [the costs](#the-costs-read-these)
before you choose it; for several languages the sidecar is the better answer and
saying so is part of the job.

---

## The ABI

Six functions. The header is [`include/llmux.h`](include/llmux.h) and it is the
supported surface — `go build -buildmode=c-shared` also emits a `libllmux.h`
next to the library, but it drags in Go's typedefs and drops the `const`
qualifiers.

```c
const char* llmux_abi_version(void);

uint64_t llmux_new(const char* config_json, char** err);
void     llmux_close(uint64_t h);

char*    llmux_call(uint64_t h, const char* method, const char* request_json, char** err);
void     llmux_free(char* p);

typedef int (*llmux_chunk_cb)(const char* chunk_json, void* user_data); /* 0 = continue */
int llmux_stream(uint64_t h, const char* method, const char* request_json,
                 llmux_chunk_cb cb, void* user_data, char** err);
```

**Requests and responses are JSON — the same JSON the HTTP API uses.** A body
that works against `POST /v1/chat/completions` works here unchanged, and what
comes back is what that endpoint returns. That is deliberate: it reuses a wire
contract that is already tested and documented, and it keeps the surface small
enough to bind from a dozen languages.

`method` is a string rather than one C function per operation, so the header
stays stable as llmux grows methods.

| function      | method     | request                        | result                    |
| ------------- | ---------- | ------------------------------ | ------------------------- |
| `llmux_call`  | `"chat"`   | OpenAI chat-completions body   | a chat completion object  |
| `llmux_call`  | `"embed"`  | OpenAI embeddings body         | an embeddings response    |
| `llmux_call`  | `"models"` | ignored — pass `NULL`          | an OpenAI model list      |
| `llmux_stream`| `"chat"`   | OpenAI chat-completions body   | one chunk per callback    |

A `"chat"` request with `"stream": true` is **refused** by `llmux_call` rather
than quietly served as one blob after the fact. Use `llmux_stream`.

### Rules

**Ownership.** Every non-const `char*` this library returns — results *and*
error messages — is freed with `llmux_free`, and with nothing else.
`llmux_abi_version` is the single exception: it returns a static string you must
not free. `llmux_free(NULL)` is safe.

**Errors.** Fallible functions take a trailing `char** err`. On failure they
write a malloc'd, human-readable UTF-8 message there; pass `NULL` if you do not
want it. The message is **not** JSON — do not parse it.

**Handles are integers in a registry inside the library, never pointers**, and
they are never reused. Calling with a closed or invented handle is a clean error
string, not a segfault in your process. `llmux_close` is idempotent.

**Threading.** A handle is safe to use from several threads at once.

**Construction is inert.** `llmux_new` starts no goroutines and — unless your
configuration names a Postgres DSN, which connects and migrates eagerly — opens
no sockets. There is no background price-catalog sync and no spend flusher in
library mode: a shared library loaded into someone else's process must not start
background traffic they did not ask for. If you want that, run the sidecar.

**Configuration.** `config_json` is an llmux configuration document — the same
JSON `llmux serve` reads from `llmux.json`. `NULL` or `""` means built-in
defaults plus the `LLMUX_`-namespaced environment (auto-detected providers,
`LLMUX_*` overrides).

**Your document wins over the environment.** A field your document states is
never overwritten by an environment variable — including one you set to `""` or
`0`, which is a statement and not a gap. Variables fill in only what the
document is silent about. The sidecar resolves the other way round (env last),
and the difference is deliberate: there the operator owns the file *and* the
environment, here the environment belongs to your application and llmux is a
guest in it.

**`DATABASE_URL` and `VULOS_DATABASE_URL` are ignored in library mode.** They
are your application's variables. `cfg.postgres` is the one field that turns an
inert construction into remote I/O — the Postgres key store connects and runs
`CREATE SCHEMA` / `CREATE TABLE` immediately — so adopting a DSN llmux was never
handed would mean migrating your production database because you loaded a shared
library. `LLMUX_POSTGRES` is namespaced, unambiguous and still honoured; to use
a database from a host that sets `DATABASE_URL`, put the DSN in your document.

**Authentication.** There is none on this boundary, by design. Virtual keys,
budgets and per-key model allow-lists are enforced by the HTTP shell's auth
middleware; an in-process host is already inside the trust boundary and calls
the gateway directly, the same as a Go embedder does. If you need per-tenant
keys and budgets, that is the sidecar's job.

### Streaming

`llmux_stream` blocks until the stream ends and calls `cb` once per chunk. Each
`chunk_json` is one `chat.completion.chunk` object — byte for byte what the HTTP
API writes after `data: ` in its SSE stream, without the prefix and without the
terminal `[DONE]` frame. **The string is owned by the library and valid only for
the duration of the callback; copy it if you need it afterwards.**

Return values: `0` on success, `-1` on failure with `*err` set.

A callback that returns non-zero **stops the stream and is not a failure**: the
return is `0` and `*err` is untouched. You returned non-zero, so you already
know it happened. Tokens already served are metered either way.

**Which thread the callback runs on.** It runs on the thread that called
`llmux_stream`, synchronously, before `llmux_stream` returns.
[`ctest/smoke.c`](ctest/smoke.c) asserts this by comparing `pthread_self()`
inside the callback against the caller's, on every CI run, because a claim about
threads that nobody measures is how bindings get written against a fiction.

That said, it is still a callback into your host from inside a Go call frame:

- **Python** must reacquire the GIL in the callback (`ctypes.CFUNCTYPE` does
  this for you; `PYFUNCTYPE` does not).
- **JVM** hosts must have the thread attached to the VM.
- If your language's FFI cannot safely accept a C callback at all, **use the
  sidecar's SSE stream for streaming instead of pretending.** A binding that
  buffers the whole answer and replays it as fake chunks is worse than an honest
  HTTP call, because it looks right until someone measures time to first token.

---

## The costs (read these)

`-buildmode=c-shared` is not free, and none of this belongs in a footnote.

1. **The Go runtime lives in your process.** Its garbage collector, its
   scheduler, and its signal handlers. Go replaces five — `SIGSEGV`, `SIGBUS`,
   `SIGFPE`, `SIGPIPE`, `SIGURG` — and leaves three more in place with
   `SA_ONSTACK` added (`SIGILL`, `SIGXFSZ`, `SIGUSR2`). A host with its own
   handling — a JVM, sanitizers, crash reporters — can conflict with it. Go
   chains to a pre-existing handler in most cases, but "most" is the honest
   word. **`SIGPROF` is not touched**, so sampling profilers are not the hazard
   here; the measured breakdown is in
   [`sdks/java/README.md`](../sdks/java/README.md#the-jvm-and-gos-signal-handlers).

2. **It is not fork-safe, and the failure is a false green.** After `fork()`
   without `exec()` the Go runtime in the child is broken: its threads did not
   come across. It does not fail loudly on the first call. Measured in real
   php-fpm, and again after `os.fork()` in Python, a broken child answers
   `models` — served from memory — in about 0.1 ms and then never answers `chat`
   at all. **A health check that only lists models will call a broken worker
   healthy.** This bites:
   - Python `multiprocessing` with the default `fork` start method on Linux —
     use `spawn` (`multiprocessing.set_start_method("spawn")`).
   - **php-fpm** in every `pm` mode, **uWSGI**, **Unicorn** and any other
     pre-fork worker model — load the library *after* the fork, in the worker,
     never in the master.

3. **Building it needs cgo and a C toolchain for each target platform.**
   Consumers only need the prebuilt artifact, but somebody builds it, per
   platform, and a cgo cross-compile needs a cross C compiler — not just a
   `GOOS` variable. See [Building](#building).

4. **It is a big artifact.** Measured, not estimated (see below): ~12 MB on
   darwin/arm64, ~17 MB on linux/arm64. `-ldflags="-s -w"` takes the darwin
   build to ~11 MB. A shared library is not free.

5. **Two libraries, two runtimes.** If your process loads libllmux *and* another
   Go-built c-shared library, you get two independent Go runtimes with two GCs.
   It works; it is not free either.

**When the sidecar is the better answer.** If your host pre-forks, if it embeds
another runtime with strong opinions about signals, if you need per-tenant keys
and budgets enforced at the boundary, or if your FFI cannot take a callback —
run `llmux serve` and talk HTTP to it. The SDKs in `sdks/` can spawn and manage
it for you so the user never runs a server by hand. Choosing the sidecar is a
supported outcome of reading this page, not a failure.

---

## Latency: in-process vs loopback HTTP

Measured, not asserted. [`bench/`](bench) dlopens the real shared library and
drives it from the same Go program that drives an `llmux serve` HTTP handler
over loopback, against the same fake upstream — so the difference is the
transport, not two HTTP clients written in different languages.

```
make ffi-bench
```

darwin/arm64 (Apple silicon laptop), 1000 measured requests each, Go 1.25.12,
llmux 0.1.2, median of three runs:

| workload                                  | in-process (C ABI) | loopback HTTP | saved  |
| ----------------------------------------- | ------------------ | ------------- | ------ |
| `models` — answered from memory, no upstream | **~4.0 µs**     | ~46–49 µs     | ~43 µs |
| `chat` — includes the upstream round trip | ~80–92 µs          | ~102–109 µs   | ~10–28 µs |

Read it this way:

- The **`models` row is the boundary itself** — a cgo call plus JSON in and JSON
  out, against a loopback TCP round trip plus HTTP framing. About **4 µs versus
  about 47 µs**. That is the cost that does not go away.
- The **`chat` row is the same saving inside a request that actually does
  something.** Both rows include an identical upstream call, so the ~20 µs delta
  is the same delta as above, diluted.
- **Against a real model this is noise.** A real completion takes hundreds of
  milliseconds to tens of seconds. Saving 40 µs of transport is a rounding error
  on that, and anyone choosing in-process *for the latency* is optimising the
  wrong thing.

So what is in-process actually for? No second process to supervise, no port to
bind, no loopback surface to secure, no request bodies crossing a socket, and
per-request state (`Result.Provider`, `Result.CacheHit`, `Result.BYOK`) that the
HTTP shell flattens into a response. Latency is the least of it. The loopback
HTTP row above is measured with keep-alive on, i.e. the sidecar at its best.

---

## Building

```
scripts/build-ffi.sh              # this host only
scripts/build-ffi.sh --all        # plus every target with a real toolchain here
scripts/build-ffi.sh --out DIR    # default: dist/ffi
```

Output:

```
dist/ffi/<goos>_<goarch>/libllmux.so | libllmux.dylib | llmux.dll
dist/ffi/<goos>_<goarch>/libllmux.h    (cgo-generated)
dist/ffi/include/llmux.h               (the stable hand-written header)
```

The script only attempts a target when it can find a C compiler that can
actually produce that target's objects, and it prints, by name and with a
reason, every target it skipped. `--all` on a machine with no cross toolchain
builds exactly one library and says so.

### What was actually built, and where

Honest status at the time of writing, so nobody reads a supported matrix into a
loop that ran three times:

| target        | status                                                                 |
| ------------- | ---------------------------------------------------------------------- |
| darwin/arm64  | **Built and tested on the development machine.** 12,787,504 bytes; the C smoke test passes all 32 checks against it. |
| linux/arm64   | **Built and tested in a `golang:1.25` container** on that same machine. 17,348,392 bytes; the C smoke test passes all 32 checks against it. |
| linux/amd64   | **Built and tested in CI** (the `ffi` job on `ubuntu-latest`). Not produced on the development machine — no cross toolchain there. |
| windows/amd64 | **Not built. Not tested. No `.dll` has been produced by anyone yet.** `build-ffi.sh` will attempt it if `x86_64-w64-mingw32-gcc` or `zig` is present; nobody has run that, so treat Windows as unverified. |
| darwin/amd64  | Not built — no Intel macOS machine or SDK here.                        |

Getting a cross toolchain, if you want one:

- linux/amd64 from Debian/Ubuntu: `apt install gcc-x86-64-linux-gnu`
- windows/amd64: `apt install gcc-mingw-w64-x86-64` (or install `zig`, which
  `build-ffi.sh` will use as `zig cc -target x86_64-windows-gnu`)
- Easiest reliable path for Linux artifacts from a Mac: build in a container,
  which is exactly what the linux/arm64 row above did.

---

## Testing

```
make test-ffi     # gated Go unit tests + the C smoke test
```

Two layers, and neither replaces the other:

- **[`abi_test.go`](abi_test.go)** — 14 tests in pure Go, no cgo. Handle
  registry, use-after-close, method dispatch, streaming, abort semantics, two
  independent gateways in one process, and the version constant against
  `../VERSION`.
- **[`ctest/smoke.c`](ctest/smoke.c)** — dlopens the built library, resolves all
  six symbols by name, and runs one unary call and one streaming call end to
  end. This is the layer that catches a missing `//export`, a renamed symbol, or
  a header that has drifted from the library — none of which any Go test would
  notice. It asserts 32 named checks **and then asserts that 32 checks ran**,
  because a C program that returns 0 having executed three of them looks
  identical to one that executed all of them.

Both run in CI (the `ffi` job), through `scripts/go-test-gate.sh`, which fails a
run in which zero tests executed — the classic false green for a nested module.

The version probe is worth spelling out: `llmux_abi_version` returns the llmux
version the library was built from, `abi_test.go` asserts that constant equals
`../VERSION`, and the C smoke test asserts the **built library** reports it. So
a stale `libllmux` earlier on your load path is detectable by a host at startup
instead of misbehaving in ways that look like llmux bugs.

## Layout

```
ffi/
  doc.go            why this is a separate module, outside the llmux import prefix
  abi.go            the whole implementation, in pure Go
  cshared.go        the cgo shim: //export wrappers and the callback trampoline
  include/llmux.h   the stable, hand-written header
  ctest/smoke.c     the C smoke test
  fakeupstream/     a fake OpenAI upstream, shared by the tests and the benchmark
  bench/            in-process vs loopback HTTP latency
  internal/fake/    the fake upstream's implementation
```

`ffi/` is a **separate Go module** whose path is `github.com/vul-os/llmux-ffi`,
deliberately outside `github.com/vul-os/llmux/`. Both facts are load-bearing:
the separate module keeps cgo out of the repo-root `go build ./...` and the
`-tags noui` build, and the path outside the prefix means Go's `internal/` rule
applies to this code exactly as it does to any third-party embedder — so the C
ABI is *evidence* that llmux's exported API is sufficient, not a privileged
insider. `TestFFIUsesOnlyThePublicAPI` asserts both.

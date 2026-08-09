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

## Six functions

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

A `"chat"` request with `"stream": true` is **refused** by `llmux_call` rather
than quietly served as one blob after the fact. Use `llmux_stream`.

## The rules

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
environment (auto-detected providers, `LLMUX_*` overrides), exactly as a missing
config file does. It goes through `config.FromJSON`, so the caveats from the Go
side apply verbatim across this boundary:

- **A configuration naming a Postgres DSN connects and migrates eagerly** during
  `llmux_new`. That is the one thing that makes construction non-inert, and it
  is an explicit opt-in you wrote into the config you passed.
- **Any provider configured with `api_key_env` is read from the environment**
  during `llmux_new`, because that is what the field means. If your host process
  scrubs or rewrites its environment before loading the library, the credential
  is not there to read.
- **`LLMUX_*` environment overrides apply** whether or not you passed a config
  document — a `NULL` config is "defaults plus environment", not "nothing".

## Streaming

`llmux_stream` blocks until the stream ends and calls `cb` once per chunk. Each
`chunk_json` is one `chat.completion.chunk` object — byte for byte what the HTTP
API writes after `data: ` in its SSE stream, without the prefix and without the
terminal `[DONE]` frame. **The string is owned by the library and valid only for
the duration of the callback; copy it if you need it afterwards.**

Returns `0` on success, `-1` on failure with `*err` set. A callback that returns
non-zero **stops the stream and is not a failure**: the return is `0` and `*err`
is untouched. You returned non-zero, so you already know it happened. Tokens
already served are metered either way.

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

## The costs

`-buildmode=c-shared` is not free, and none of this belongs in a footnote.

1. **The Go runtime lives in your process** — its garbage collector, its
   scheduler, and its signal handlers. Go installs handlers for `SIGSEGV`,
   `SIGBUS`, `SIGFPE`, `SIGPROF` and others. A host with its own handling — a
   JVM, some Python profilers, sanitizers, crash reporters — can conflict with
   it. Go chains to a pre-existing handler in most cases, but "most" is the
   honest word.

2. **It is not fork-safe.** After `fork()` without `exec()`, the Go runtime in
   the child is broken: its threads did not come across, so the first call into
   the library can hang or crash. This bites:
   - Python `multiprocessing` with the default `fork` start method on Linux —
     use `multiprocessing.set_start_method("spawn")`.
   - **uWSGI**, **Unicorn**, and any other pre-fork worker model — load the
     library *after* the fork, in the worker, never in the master.

3. **Building it needs cgo and a C toolchain per target platform.** Consumers
   only need the prebuilt artifact, but somebody builds it, per platform, and a
   cgo cross-compile needs a cross C compiler — not just a `GOOS` variable.

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
| **darwin/arm64** | Built and tested on the development machine. 12,769,346 bytes; the C smoke test passes all 32 checks against it |
| **linux/arm64** | Built and tested in a `golang:1.25` container on that same machine. 17,348,392 bytes; all 32 checks pass |
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
- **`ffi/ctest/smoke.c`** — dlopens the built library, resolves all six symbols
  by name, and runs one unary call and one streaming call end to end. This is
  the layer that catches a missing `//export`, a renamed symbol, or a header
  that has drifted from the library — none of which any Go test would notice. It
  asserts 32 named checks **and then asserts that 32 checks ran**, because a C
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

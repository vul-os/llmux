# llmux from C

C is the ground truth here. `llmux_call`, `llmux_stream` and the rest are a **C
ABI**; every other binding in `sdks/` — Python's ctypes, Ruby's fiddle, Java's
FFM, .NET's `DllImport` — is doing what `direct_chat.c` does, wrapped in that
language's ceremony. If you want to know what a call really costs or what
really owns a pointer, read the C.

There is no library to install for C. The header is
[`ffi/include/llmux.h`](../../ffi/include/llmux.h) and it is the whole surface:
seven functions.

```c
const char* llmux_abi_version(void);
uint64_t    llmux_new(const char* config_json, char** err);
void        llmux_close(uint64_t h);
void        llmux_cancel(uint64_t h);
char*       llmux_call(uint64_t h, const char* method, const char* request_json, char** err);
void        llmux_free(char* p);
int         llmux_stream(uint64_t h, const char* method, const char* request_json,
                         llmux_chunk_cb cb, void* user_data, char** err);
```

`llmux_cancel` is the seventh symbol, added in 0.1.5: it aborts every call in
flight on a handle **without closing it**, which is the only way to abandon a
blocked `llmux_call` or `llmux_stream`. Before it, the only escape was
`llmux_close`, which destroys the gateway and every other stream on it.

Two things about `llmux_close` also changed in 0.1.5, and both matter to a C
host: it **drains**, waiting up to a 5 s grace for in-flight calls to return
before releasing the Redis client and Postgres pool, so it can block; and it
**must not be called from inside a chunk callback**, which would be waiting on
the call that is running it. Return from the callback first.

## The two examples

| file | mode | what it shows |
|---|---|---|
| `direct_chat.c` | direct | libllmux linked into the program: version probe, `models`, `chat`, streaming, stopping a stream early, cancelling one from another thread and from inside the callback, the error path, one cleanup label |
| `sidecar_chat.c` | sidecar | spawn `llmux` on a free loopback port, poll `/health`, unary chat, SSE streaming, HTTP errors, kill the child on every path |

```bash
./run-demo.sh                 # build and run both against a fake upstream
./run-demo.sh direct_chat     # just one
make                          # build only
```

`run-demo.sh` starts `ffi/fakeupstream`, the same OpenAI-compatible fake the Go
tests, the C smoke test and the latency benchmark use, so both examples run with
no provider account and no network. It builds `libllmux` and the `llmux` binary
if they are not in `dist/` yet (that part needs a Go toolchain).

`run-demo.sh` also starts a **second**, unrelated fake upstream —
[`sdks/fake-upstream.py`](../fake-upstream.py) — just for `direct_chat.c`'s
cancellation demo. `ffi/fakeupstream` answers instantly and counts nothing, so
there is no window in it to cancel into and nothing to check a cancellation
against; `fake-upstream.py` sleeps `--chunk-delay-ms` between chunks and
answers `GET /generated` with what it actually wrote to the socket. Needs
`python3` on `PATH`; nothing else in this script does.

## Cancellation

`llmux_cancel` aborts every call in flight on a handle without closing it —
the only way to get a *blocked* `llmux_call` or `llmux_stream` to return short
of destroying the whole gateway with `llmux_close`. `direct_chat.c` runs it two
ways, both against `fake-upstream.py` so the numbers below are measured, not
assumed:

- **From another thread**, with a `pthread_create`d canceller waiting on a
  condition variable until three chunks have arrived, then calling
  `llmux_cancel` while the main thread is still blocked inside `llmux_stream`.
- **From inside the chunk callback**, the route a single-threaded host (Node,
  PHP, a fiber) has to use, because the callback is the only code of theirs
  that runs while `llmux_stream` blocks. **Verified safe**: unlike
  `llmux_close`, which would deadlock waiting on the very call running the
  callback that invoked it, `llmux_cancel` does not wait.

Both routes return the same way: `llmux_stream` returns `-1` with `*err` set to
exactly `context canceled` — a **failure** return, unlike a callback returning
non-zero, which returns `0` with no error. Chunks already delivered stay
delivered, and the handle survives: `direct_chat.c` calls `llmux_call(h,
"models", ...)` right after each cancel and it succeeds.

Measured on this machine, against `fake-upstream.py --chunk-delay-ms 100`
serving ten words (twelve `chat.completion.chunk` frames: ten words, one
finish frame, one usage frame):

```
cancel        (another thread)   stream returned -1, err context canceled
                                  consumer saw 3 chunks: "one two three "
                                  upstream GET /generated: 3 of 12
cancel        (inside callback)  stream returned -1, err context canceled
                                  consumer saw 3 chunks: "one two three "
                                  upstream GET /generated: 3 of 12
```

The consumer stopped at 3; the upstream generated 3 of 12 — for *both* routes.
That is the whole point of measuring against `GET /generated` rather than
trusting the return value: a cancellation that returns promptly to the caller
while the provider keeps generating (and llmux keeps metering) in the
background would look identical from the consumer's side, and this is the
number that tells the two apart.

This does **not** mean returning non-zero from the callback ("stopping early",
demonstrated earlier in `direct_chat.c`) leaves the provider running — it does
not: the library closes the upstream connection either way, and the provider
observes a client disconnect and stops. The difference is which door is open
to you. Returning non-zero from the callback only works from *inside* a chunk
you already have; `llmux_cancel` is for abandoning a call from somewhere else
entirely — another thread, a timeout, a request that hung up — with no chunk
to return from.

**`llmux_cancel` is per-HANDLE, not per-call.** It aborts *every* call in
flight on that gateway, not only the one you have in mind. `direct_chat.c`'s
two demos above run one after another on the same handle for exactly this
reason: had they run concurrently, cancelling one would have killed the other
too. If your program runs several streams at once and needs to cancel one
without disturbing the rest, give it its own gateway — one gateway per
cancellation scope.

**These are examples, not tests.** The test is
[`ffi/ctest/smoke.c`](../../ffi/ctest/smoke.c): it `dlopen()`s the library,
resolves all seven symbols **by name**, and asserts 40 checks and then asserts
that 40 checks ran. That is what catches a missing `//export`, a renamed symbol
or a header that has drifted from the library — a different job from showing
someone how to call this. If you change the ABI, that file is the one that must
fail. These examples deliberately link the library instead of `dlopen`ing it,
because that is how a program with an installed library is actually written.

## Building against libllmux

```
cc -std=c11 -I<repo>/ffi/include -o direct_chat direct_chat.c jsonpeek.c mini_http.c \
   -L<libdir> -lllmux -Wl,-rpath,<libdir> -lpthread
```

Build the library first: `scripts/build-ffi.sh` from the repo root writes
`dist/ffi/<goos>_<goarch>/`. Point the Makefile elsewhere with
`make LLMUX_LIB_DIR=/path/to/dir`.

**macOS wart, worth knowing before it bites you.** `go build
-buildmode=c-shared` gives the dylib the bare install name `libllmux.dylib`
with no `@rpath/` prefix, so `-rpath` is never consulted and the program dies at
startup with `Library not loaded: libllmux.dylib`. The Makefile works around it
in the executable:

```
install_name_tool -change libllmux.dylib @rpath/libllmux.dylib direct_chat
```

If you are packaging the library rather than consuming it from a checkout, fix
it at the source instead:

```
install_name_tool -id @rpath/libllmux.dylib libllmux.dylib
```

## The rules, in C terms

**Ownership.** Every non-`const char*` the library returns — results *and* error
messages — is freed with `llmux_free` and nothing else. Not `free()`: it was not
allocated by your allocator. `llmux_free(NULL)` is safe, which is why the
cleanup block in `direct_chat.c` has no null checks. `llmux_abi_version` is the
one exception; its string is static, so do not free it.

**Errors.** Fallible functions take a trailing `char** err`. The message is
plain UTF-8 **text, not JSON** — print it, do not parse it — and it is yours to
free.

**No RAII, so: one cleanup label.** `direct_chat.c` has exactly one `goto done`
target and no early `return` after the handle exists. That is the C shape of the
guarantee every other SDK in this repo makes with a context manager, `defer`,
`using` or a destructor.

**Handles are integers in a registry inside the library, never pointers**, and
are never reused. Calling with a closed or invented handle is a clean error
string, not a segfault in your address space — `direct_chat.c` demonstrates both.
`llmux_close` is idempotent.

**Threading.** A handle is safe to use from several threads at once.

**Streaming.** `llmux_stream` blocks and calls your callback once per chunk, on
**the thread that called it**, before it returns. `direct_chat.c` compares
`pthread_self()` inside the callback and prints the verdict; `smoke.c` asserts
it on every CI run. `chunk_json` is owned by the library and valid only for the
duration of the call — copy it. Returning non-zero stops the stream and is **not**
an error: the return is 0 and `*err` is untouched. `llmux_cancel` is the other
way to stop one — from outside the callback entirely, with a failure return
rather than a clean one — see [Cancellation](#cancellation) above.

## Which mode to use from C

C is the one language where direct mode has no FFI friction to speak of — no
marshalling layer, no runtime to attach, no GIL. Prefer it, **unless**:

- **Your process forks.** The Go runtime does not survive `fork()` without
  `exec()`; the child hangs on the first call that needs a thread that did not
  come across. `sidecar_chat.c` forks, and is safe doing so *only because it
  never loads libllmux*. Do not merge the two examples into one binary.
- **Your process handles its own signals.** Measured: Go replaces `SIGSEGV`,
  `SIGBUS`, `SIGFPE`, `SIGPIPE` and `SIGURG`, and adds `SA_ONSTACK` to `SIGILL`,
  `SIGXFSZ` and `SIGUSR2`. A crash reporter or a sanitizer build can conflict. Go
  chains to a pre-existing handler in most cases; "most" is the honest word.
  **`SIGPROF` is not touched**, so a `SIGPROF`-driven sampling profiler keeps
  working, and `perf`, which uses `perf_events` rather than signals, was never at
  risk. The measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).
- **You need per-tenant keys, budgets or model allow-lists.** Those are enforced
  by the HTTP shell's auth middleware. The C ABI has no authentication by
  design: an in-process host is already inside the trust boundary.
- **You are on a platform with no library** — see the table below.

## Platform reality for llmux

| target | status |
|---|---|
| darwin/arm64 | built and smoke-tested (40/40 checks). 12,823,104 bytes |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,356,264 bytes |
| linux/amd64 | built in CI only; not produced on a development machine |
| windows/amd64 | **not built. No DLL exists.** `build-ffi.sh` will attempt it with mingw or zig; nobody has run that |
| darwin/amd64 | **not built.** |

The shared library is 12–17 MB. On a platform with no library, the sidecar is
the answer, and choosing it is a supported outcome, not a fallback.

## Latency

Measured on darwin/arm64, not asserted: the boundary itself is ~4 µs in-process
against ~46 µs over loopback HTTP; a `chat` call including the upstream round
trip is ~80–92 µs against ~102–109 µs. Against a model that answers in hundreds
of milliseconds, 40 µs is noise. **Embed for no second process, no port and no
loopback surface — not for speed.** `ffi/README.md` has the full table and the
benchmark that produced it.

## The two helper files

`jsonpeek.c` and `mini_http.c` are here so the examples have no dependencies.
Neither is a component to reuse:

- **`jsonpeek`** is not a JSON parser. It scans for `"key":` and reads what
  follows, understanding `\"` and `\\` and nothing else. Real programs link
  cJSON, jansson or yyjson — llmux speaks ordinary OpenAI JSON, so any of them
  works unchanged.
- **`mini_http`** is not an HTTP client. It does one request to `127.0.0.1` with
  `Connection: close`, plus an SSE reader. No TLS, no chunked encoding, no
  keep-alive, no redirects, no retries. Real programs link libcurl. Originally
  `sidecar_chat.c`-only; `direct_chat.c` now links it too, for the one GET
  against `fake-upstream.py`'s `/generated` in the cancellation demo — a
  request that has nothing to do with the ABI, which is why it goes through
  loopback HTTP rather than through libllmux.

# llmux from C

C is the ground truth here. `llmux_call`, `llmux_stream` and the rest are a **C
ABI**; every other binding in `sdks/` — Python's ctypes, Ruby's fiddle, Java's
FFM, .NET's `DllImport` — is doing what `direct_chat.c` does, wrapped in that
language's ceremony. If you want to know what a call really costs or what
really owns a pointer, read the C.

There is no library to install for C. The header is
[`ffi/include/llmux.h`](../../ffi/include/llmux.h) and it is the whole surface:
six functions.

```c
const char* llmux_abi_version(void);
uint64_t    llmux_new(const char* config_json, char** err);
void        llmux_close(uint64_t h);
char*       llmux_call(uint64_t h, const char* method, const char* request_json, char** err);
void        llmux_free(char* p);
int         llmux_stream(uint64_t h, const char* method, const char* request_json,
                         llmux_chunk_cb cb, void* user_data, char** err);
```

## The two examples

| file | mode | what it shows |
|---|---|---|
| `direct_chat.c` | direct | libllmux linked into the program: version probe, `models`, `chat`, streaming, aborting a stream, the error path, one cleanup label |
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

**These are examples, not tests.** The test is
[`ffi/ctest/smoke.c`](../../ffi/ctest/smoke.c): it `dlopen()`s the library,
resolves all six symbols **by name**, and asserts 32 checks and then asserts
that 32 checks ran. That is what catches a missing `//export`, a renamed symbol
or a header that has drifted from the library — a different job from showing
someone how to call this. If you change the ABI, that file is the one that must
fail. These examples deliberately link the library instead of `dlopen`ing it,
because that is how a program with an installed library is actually written.

## Building against libllmux

```
cc -std=c11 -I<repo>/ffi/include -o direct_chat direct_chat.c jsonpeek.c \
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
an error: the return is 0 and `*err` is untouched.

## Which mode to use from C

C is the one language where direct mode has no FFI friction to speak of — no
marshalling layer, no runtime to attach, no GIL. Prefer it, **unless**:

- **Your process forks.** The Go runtime does not survive `fork()` without
  `exec()`; the child hangs on the first call that needs a thread that did not
  come across. `sidecar_chat.c` forks, and is safe doing so *only because it
  never loads libllmux*. Do not merge the two examples into one binary.
- **Your process handles its own signals.** Go installs handlers for `SIGSEGV`,
  `SIGBUS`, `SIGFPE`, `SIGPROF` and others. A crash reporter, a sampling
  profiler or a sanitizer build can conflict. Go chains to a pre-existing
  handler in most cases; "most" is the honest word.
- **You need per-tenant keys, budgets or model allow-lists.** Those are enforced
  by the HTTP shell's auth middleware. The C ABI has no authentication by
  design: an in-process host is already inside the trust boundary.
- **You are on a platform with no library** — see the table below.

## Platform reality for llmux

| target | status |
|---|---|
| darwin/arm64 | built and smoke-tested (32/32 checks). 12,769,346 bytes |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,348,392 bytes |
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
  keep-alive, no redirects, no retries. Real programs link libcurl.

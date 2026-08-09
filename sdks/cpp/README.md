# llmux from C++

`llmux.hpp` is a header-only, C++17 RAII wrapper over the llmux C ABI. No
dependencies beyond the standard library and
[`ffi/include/llmux.h`](../../ffi/include/llmux.h). Copy the header into your
project, or add both directories to your include path.

```cpp
#include "llmux.hpp"

llmux::Gateway gw;                                   // env defaults; throws on failure
std::string answer = gw.chat(R"({"model":"gpt-4o-mini","messages":[...]})");

gw.stream(request, [](std::string_view chunk) {
    std::cout << chunk;
    return true;                                     // false stops the stream
});
                                                     // ~Gateway closes the handle
```

## What the wrapper is actually for

The C ABI hands out two kinds of resource: a `uint64_t` handle, and malloc'd
`char*` strings that only `llmux_free` may release. Both are easy to leak on an
error path, and in C++ **every** path is an error path, because any line can
throw. So:

- **Every `char*` the library returns is owned by `llmux::OwnedString` the
  instant it comes back** — results and error messages alike, including the
  message that is about to become an exception. There is no window in which it
  is a raw pointer, so there is no throw that can leak it.
- **The handle is owned by `llmux::Gateway`**, which is move-only and closes in
  its destructor. `close()` is `noexcept` and `llmux_close` is idempotent, so
  double-close and close-during-unwinding are both fine.
- **An exception thrown inside your stream callback is caught at the C
  boundary**, turned into "stop the stream", and rethrown once the C frame has
  returned. Letting it unwind through a Go call frame is undefined behaviour.
  `direct_chat.cpp` demonstrates all three, including the unwinding one.

## Errors: exceptions or expected-style, your choice

Both are present and neither is a second implementation — the throwing calls are
one line each on top of the non-throwing ones.

```cpp
std::string out = gw.chat(req);                  // throws llmux::Error

llmux::StringResult r = gw.try_chat(req);        // never throws
if (!r.ok()) std::cerr << r.error();             // plain UTF-8 text, not JSON
else         use(r.value());                     // or r.take() to move it out
```

`Gateway::try_open()` is the non-throwing constructor. Define
`LLMUX_NO_EXCEPTIONS` (or build with `-fno-exceptions`, which defines it for
you) to compile only the `try_` layer.

## The two examples

| file | mode | what it shows |
|---|---|---|
| `direct_chat.cpp` | direct | version probe, `models`, `chat`, streaming, aborting a stream, an exception thrown *inside* the callback, both error styles, a handle closed during stack unwinding |
| `sidecar_chat.cpp` | sidecar | `Socket` and `Sidecar` classes with destructors, spawn on a free loopback port, `/health` poll, unary chat, SSE streaming, a child terminated during unwinding |

```bash
./run-demo.sh                 # build and run both against a fake upstream
./run-demo.sh direct_chat     # just one
make                          # build only
```

`run-demo.sh` starts `ffi/fakeupstream` — the same OpenAI-compatible fake the Go
tests, the C smoke test and the latency benchmark use — so both run with no
provider account and no network. It builds `libllmux` and the `llmux` binary if
they are not in `dist/` yet (that part needs a Go toolchain).

Compare `direct_chat.cpp` with [`../c/direct_chat.c`](../c/direct_chat.c): the
same calls in the same order producing the same output, with the C version's
single `goto done` cleanup label replaced by destructors. That comparison is the
best short argument for the wrapper.

**`sidecar_chat.cpp` never includes `llmux.hpp`, and must not.** It forks, and a
process that has loaded the Go runtime cannot fork safely. Keep them as two
binaries.

## Building

```
c++ -std=c++17 -I<repo>/ffi/include -o direct_chat direct_chat.cpp \
    -L<libdir> -lllmux -Wl,-rpath,<libdir> -lpthread
```

Build the library first with `scripts/build-ffi.sh` from the repo root; it
writes `dist/ffi/<goos>_<goarch>/`. Override with
`make LLMUX_LIB_DIR=/path/to/dir`.

**macOS wart.** `go build -buildmode=c-shared` gives the dylib the bare install
name `libllmux.dylib` with no `@rpath/` prefix, so `-rpath` is never consulted
and the program dies with `Library not loaded: libllmux.dylib`. The Makefile
fixes it in the executable with
`install_name_tool -change libllmux.dylib @rpath/libllmux.dylib direct_chat`; a
packager should instead fix the library with
`install_name_tool -id @rpath/libllmux.dylib libllmux.dylib`.

## Threads

`llmux_stream` blocks and calls your callback **on the thread that called it**,
synchronously, before it returns. That is measured with `pthread_self()` in
`ffi/ctest/smoke.c` on every CI run, and `direct_chat.cpp` prints the same
verdict using `std::this_thread::get_id()`. So there is no thread-registration
dance in this header, and none was written. `chunk` is valid only for the
duration of the call — the `std::string_view` you get does not own it, so copy
what you keep.

A handle is safe to use from several threads at once. `llmux::Gateway` adds no
state of its own beyond the handle, so it is exactly as thread-safe.

## Which mode to use from C++

Like C, C++ has no FFI friction here — no marshalling, no runtime to attach, no
GIL. Prefer direct mode, **unless**:

- **Your process forks.** The Go runtime does not survive `fork()` without
  `exec()`.
- **Your process handles its own signals**, or you build with sanitizers, or you
  ship a crash reporter. Measured: Go replaces `SIGSEGV`, `SIGBUS`, `SIGFPE`,
  `SIGPIPE` and `SIGURG`, and adds `SA_ONSTACK` to `SIGILL`, `SIGXFSZ` and
  `SIGUSR2`. Go chains to a pre-existing handler in most cases; "most" is the
  honest word. **`SIGPROF` is not touched**, so a `SIGPROF`-driven sampling
  profiler keeps working, and `perf`, which uses `perf_events` rather than
  signals, was never at risk. The measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).
- **You need per-tenant keys, budgets or model allow-lists.** Those live in the
  HTTP shell's auth middleware; the C ABI has no authentication by design,
  because an in-process host is already inside the trust boundary.
- **You are on a platform with no library** (below).

## Platform reality for llmux

| target | status |
|---|---|
| darwin/arm64 | built and smoke-tested (32/32 checks). 12,787,504 bytes |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,348,392 bytes |
| linux/amd64 | built in CI only |
| windows/amd64 | **not built. No DLL exists.** |
| darwin/amd64 | **not built.** |

The shared library is 12–17 MB. Where there is no library, use the sidecar —
a supported answer, not a fallback.

## Latency

Measured on darwin/arm64: the boundary itself is ~4 µs in-process against ~46 µs
over loopback HTTP, and a `chat` call including the upstream round trip is
~80–92 µs against ~102–109 µs. A real model answers in hundreds of milliseconds,
so 40 µs is noise. **Embed for no second process, no port and no loopback
surface — not for speed.**

## Reference

```cpp
namespace llmux {

std::string abi_version();                       // static string; never freed

class OwnedString {                              // move-only; frees with llmux_free
    const char* c_str() const noexcept;
    std::string str() const;
    char** err_slot() noexcept;                  // for a trailing char** err
};

template <class T> class Result {                // value held in a std::optional,
    bool ok() const;                             // so a failure constructs no T
    const std::string& error() const;
    const T& value() const&;   T take();
};
using StringResult = Result<std::string>;
using VoidResult   = Result<Unit>;

class Error : public std::runtime_error {};      // carries the library's message

class Gateway {                                  // move-only; closes on destruction
    static Result<Gateway> try_open(const char* config_json = nullptr);
    explicit Gateway(const char* config_json = nullptr);        // throws
    std::uint64_t handle() const noexcept;
    bool is_open() const noexcept;
    void close() noexcept;                       // idempotent

    StringResult try_call(const char* method, const char* request_json);
    StringResult try_chat(const char*);
    StringResult try_embed(const char*);
    StringResult try_models();
    VoidResult   try_stream(const char* request_json, const ChunkFn&);

    std::string call(const char* method, const char* request_json);   // throwing
    std::string chat(const char*);   std::string chat(const std::string&);
    std::string embed(const char*);  std::string embed(const std::string&);
    std::string models();
    void        stream(const char* request_json, const ChunkFn&);

    using ChunkFn = std::function<bool(std::string_view)>;  // false stops
};

}
```

`"stream": true` passed to `chat()` is refused rather than quietly batched. Use
`stream()`.

The JSON is not parsed for you, and that is deliberate: llmux speaks ordinary
OpenAI JSON, so use whatever your project already uses — nlohmann/json,
simdjson, RapidJSON. The examples use a twelve-line string scanner for printing
only, and say so where it is defined.

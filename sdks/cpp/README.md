# llmux from C++

`llmux.hpp` is a header-only, C++17 RAII wrapper over the llmux C ABI — seven
functions, all listed in [`ffi/include/llmux.h`](../../ffi/include/llmux.h).
No dependencies beyond the standard library. Copy the header into your
project, or add both directories to your include path.

Building as **C++20** additionally unlocks a `stream(..., std::stop_token)`
overload — see [Cancellation](#cancellation) — but C++17 loses nothing else:
the rest of the wrapper, including the plain `cancel()`, is C++17 throughout.

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
  double-close and close-during-unwinding are both fine. Since 0.1.5 that
  destructor can **block up to 5 s**: `llmux_close` cancels the calls in flight
  and waits for them to return before releasing the Redis client and Postgres
  pool. Do not destroy a `Gateway` from inside a stream callback — that would
  wait on the call running the callback. Return from the callback first.
- **An exception thrown inside your stream callback is caught at the C
  boundary**, turned into "stop the stream", and rethrown once the C frame has
  returned. Letting it unwind through a Go call frame is undefined behaviour.
  `direct_chat.cpp` demonstrates all three, including the unwinding one.
- **`cancel()` aborts a blocked call from outside it** — see
  [Cancellation](#cancellation) below for the full story, including the
  `std::jthread`/`std::stop_token` overload C++20 adds on top of it.

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

## Cancellation

`llmux_cancel` aborts every call in flight on a handle without closing it —
the only way to get a *blocked* `try_call`/`try_stream` to return short of
destroying the whole `Gateway`. Two ways to reach it, both wired up in
`direct_chat.cpp` against `fake-upstream.py` so the numbers below are
measured, not assumed:

```cpp
gw.cancel();                                   // the raw primitive: fire and
                                                // forget, from any thread, or
                                                // from inside the chunk
                                                // callback itself

// C++20 only:
std::stop_source stopper;
std::jthread watcher([&](std::stop_token) { /* ... */ stopper.request_stop(); });
gw.stream(req, on_chunk, stopper.get_token());  // registers a std::stop_callback
                                                // that calls cancel() for the
                                                // duration of this call
```

`cancel()` is the primitive; the `stop_token` overload is a thin layer on top
that registers a `std::stop_callback` for the duration of `try_stream`/`stream`
and lets it go — no condition variable of your own required to hand the signal
across threads. `direct_chat.cpp` demonstrates the raw primitive first (a
`std::thread` playing "canceller", synchronized with a `std::mutex` and
`std::condition_variable`) and then the `std::jthread` idiom, to show what the
overload buys you.

Both routes return the same way: `try_stream` comes back `!ok()` with
`error() == "context canceled"` — a **failure**, unlike a callback returning
`false`, which is `ok()` with no error. Chunks already delivered stay
delivered, and the handle survives: `direct_chat.cpp` calls `try_models()`
right after each cancel and it succeeds.

Measured on this machine, against `fake-upstream.py --chunk-delay-ms 100`
serving ten words (twelve `chat.completion.chunk` frames: ten words, one
finish frame, one usage frame):

```
cancel        (cancel(), another thread)  ok=false error="context canceled"
                                          consumer saw 3 chunks: "one two three "
                                          upstream GET /generated: 3 of 12
cancel        (jthread + stop_token)      ok=false error="context canceled"
                                          consumer saw 3 chunks: "one two three "
                                          upstream GET /generated: 3 of 12
```

The consumer stopped at 3; the upstream generated 3 of 12 — for *both* routes.
That is the whole point of measuring against `GET /generated` rather than
trusting the return value: a cancellation that returns promptly to the caller
while the provider keeps generating (and llmux keeps metering) in the
background would look identical from the consumer's side, and this is the
number that tells the two apart.

This does **not** mean returning `false` from the callback ("stopping early",
demonstrated earlier in `direct_chat.cpp`) leaves the provider running — it
does not: the library closes the upstream connection either way, and the
provider observes a client disconnect and stops. The difference is which door
is open to you. Returning `false` only works from *inside* a chunk you already
have; `cancel()` is for abandoning a call from somewhere else entirely —
another thread, a timeout, a request that hung up — with no chunk to return
from.

**Both are per-HANDLE, not per-stream.** `cancel()`, and a stop request on a
token passed to `stream()`, abort *every* call in flight on that `Gateway`, not
only the one you have in mind — there is no per-stream cancellation token at
the C ABI. `direct_chat.cpp`'s two demos run one after another on the same
`Gateway` for exactly this reason: had they run concurrently, cancelling one
would have killed the other too. Give a stream its own `Gateway` if you need to
cancel it alone.

## The two examples

| file | mode | what it shows |
|---|---|---|
| `direct_chat.cpp` | direct | version probe, `models`, `chat`, streaming, stopping a stream early, cancelling one with `cancel()` from another thread and with a `std::jthread`'s `std::stop_token`, an exception thrown *inside* the callback, both error styles, a handle closed during stack unwinding |
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

`run-demo.sh` also starts a **second**, unrelated fake upstream —
[`sdks/fake-upstream.py`](../fake-upstream.py) — just for `direct_chat.cpp`'s
cancellation demo. `ffi/fakeupstream` answers instantly and counts nothing, so
there is no window in it to cancel into and nothing to check a cancellation
against; `fake-upstream.py` sleeps `--chunk-delay-ms` between chunks and
answers `GET /generated` with what it actually wrote to the socket. Needs
`python3` on `PATH`; nothing else in this script does.

Compare `direct_chat.cpp` with [`../c/direct_chat.c`](../c/direct_chat.c): the
same calls in the same order producing the same output, with the C version's
single `goto done` cleanup label replaced by destructors. That comparison is the
best short argument for the wrapper.

**`sidecar_chat.cpp` never includes `llmux.hpp`, and must not.** It forks, and a
process that has loaded the Go runtime cannot fork safely. Keep them as two
binaries.

## Building

```
c++ -std=c++20 -I<repo>/ffi/include -o direct_chat direct_chat.cpp \
    -L<libdir> -lllmux -Wl,-rpath,<libdir> -lpthread
```

The Makefile builds at `-std=c++20` deliberately, raised from C++17 for exactly
one reason: `direct_chat.cpp` demonstrates the `std::jthread`/`std::stop_token`
cancellation overload, which does not exist below C++20. `llmux.hpp` itself is
unaffected either way — the overload is feature-tested on `__cpp_lib_jthread`
and is simply absent, not a compile error, if you build your own program at
`-std=c++17`.

Build the library first with `scripts/build-ffi.sh` from the repo root; it
writes `dist/ffi/<goos>_<goarch>/`. Override with
`make LLMUX_LIB_DIR=/path/to/dir`.

**macOS wart #1.** `go build -buildmode=c-shared` gives the dylib the bare
install name `libllmux.dylib` with no `@rpath/` prefix, so `-rpath` is never
consulted and the program dies with `Library not loaded: libllmux.dylib`. The
Makefile fixes it in the executable with
`install_name_tool -change libllmux.dylib @rpath/libllmux.dylib direct_chat`; a
packager should instead fix the library with
`install_name_tool -id @rpath/libllmux.dylib libllmux.dylib`.

**macOS wart #2, C++20-specific.** Apple's libc++ (Xcode/CLT through at least
clang 17.0.0, measured on macOS 15.7) ships `<stop_token>` but guards the
actual `std::stop_source`/`std::stop_token`/`std::jthread` definitions behind
`_LIBCPP_HAS_NO_EXPERIMENTAL_STOP_TOKEN`, which is defined — i.e. the feature
is compiled OUT — unless you pass `-fexperimental-library`. Without that flag,
`__cpp_lib_jthread` is never defined, `llmux.hpp`'s stop_token overload quietly
does not exist, and `direct_chat.cpp`'s demo of it does not compile at all
(`std::jthread`/`std::stop_source` are used directly there, not through the
header). The Makefile passes the flag on Darwin only — passing it to a
compiler that does not recognise it is a hard error, and neither GCC's
libstdc++ nor a recent upstream LLVM libc++ needs it.

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
| darwin/arm64 | built and smoke-tested (40/40 checks). 12,823,104 bytes |
| linux/arm64 | built and smoke-tested in a `golang:1.25` container. 17,356,264 bytes |
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
    void cancel() noexcept;                      // no-op if nothing is running

    StringResult try_call(const char* method, const char* request_json);
    StringResult try_chat(const char*);
    StringResult try_embed(const char*);
    StringResult try_models();
    VoidResult   try_stream(const char* request_json, const ChunkFn&);
    VoidResult   try_stream(const char* request_json, const ChunkFn&,
                            std::stop_token);      // C++20 only, __cpp_lib_jthread

    std::string call(const char* method, const char* request_json);   // throwing
    std::string chat(const char*);   std::string chat(const std::string&);
    std::string embed(const char*);  std::string embed(const std::string&);
    std::string models();
    void        stream(const char* request_json, const ChunkFn&);
    void        stream(const char* request_json, const ChunkFn&, std::stop_token);
    void        stream(const std::string&, const ChunkFn&, std::stop_token);

    using ChunkFn = std::function<bool(std::string_view)>;  // false stops
};

}
```

`"stream": true` passed to `chat()` is refused rather than quietly batched. Use
`stream()`. The `std::stop_token` overloads exist only when `__cpp_lib_jthread`
is defined — see [Building](#building) for the Apple libc++ wart that gates it.

The JSON is not parsed for you, and that is deliberate: llmux speaks ordinary
OpenAI JSON, so use whatever your project already uses — nlohmann/json,
simdjson, RapidJSON. The examples use a twelve-line string scanner for printing
only, and say so where it is defined.

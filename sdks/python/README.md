# llmux for Python

One OpenAI-compatible client for every provider — routing, retries, failover,
sovereignty enforcement, BYOK, caching, pricing, metering — with **no server to
run**.

Two modes. The JSON is identical in both; only the transport differs.

| | **Sidecar** | **Direct** |
|---|---|---|
| what runs | the `llmux` binary as a child process on `127.0.0.1` | `libllmux` loaded into your interpreter with `ctypes` |
| module | `llmux._sidecar` | `llmux._direct` |
| streaming | server-sent events off a socket | a C callback, on your own thread |
| survives `fork()` | **yes** | **no** |
| per-tenant keys, budgets, model allow-lists | **yes** — enforced by the HTTP auth middleware | no, by design |
| extra bytes on disk | the binary you already have | a 12–17 MB shared library |
| platforms | wherever the binary is built | darwin/arm64 and linux/arm64 only (see below) |

## Which mode

**Start with the sidecar.** For Python specifically, it is the better default,
and the reason is not taste:

- **Python forks.** `multiprocessing` and `ProcessPoolExecutor` default to the
  `fork` start method on Linux for anything older than 3.14. uWSGI, Gunicorn's
  sync and gthread workers, and Celery's prefork pool all import your
  application in a master and then fork workers. Direct mode is **not
  fork-safe**, and `examples/fork_hazard.py` shows exactly what that costs.
- **Per-tenant keys and budgets live at the HTTP boundary.** Direct mode has no
  authentication on purpose: an in-process host is already inside the trust
  boundary. If you need virtual keys, spend limits or per-key model allow-lists,
  you need the sidecar.
- **The sidecar runs everywhere the binary does.** Direct mode needs a shared
  library built for your exact platform, and for several platforms none exists.

**Choose direct when** you want no second process to supervise, no port to bind
and no loopback surface to secure — a CLI tool, a desktop app, a notebook, a
single-process worker, an embedded appliance. Not for the latency; see below.

`patala-go/README.md` is the house precedent for saying this plainly: cackle
chose the sidecar and was right to.

## Install

> **Not `pip install llmux`.** That name on PyPI belongs to an unrelated
> project — "LLM inference for power users", by another author — so the
> instruction that used to be here installed a stranger's package and then had
> you call this API on it. This package is not published to PyPI at all yet
> (checked 2026-08-10).

Install it from a checkout:

```bash
git clone https://github.com/vul-os/llmux
pip install -e llmux/sdks/python
```

That editable install is tested, including on a tree that has never been built:
`llmux/bin/` is a build artifact, and because `pyproject.toml` force-includes it
into the wheel, hatchling used to raise `FileNotFoundError` and fail the install
outright on a fresh clone. The directory is now kept in the checkout.

Sidecar mode also needs the binary. A platform wheel bundles it at
`llmux/bin/llmux` — but no wheel is published, so from a checkout build it with
`go build -o sdks/python/llmux/bin/llmux ./cmd/llmux`, or put `llmux` on `PATH`,
or set `LLMUX_BINARY`.

Direct mode also needs the shared library, which is **not** bundled today. Build
it from a checkout (`scripts/build-ffi.sh`) and point `LLMUX_LIBRARY` at the
result, or drop it at `llmux/lib/libllmux.{dylib,so}`.

## Sidecar

```python
import llmux

client = llmux.OpenAI()                      # spawns the gateway, returns a client
r = client.chat.completions.create(
    model="anthropic/claude-3-5-sonnet",     # any provider, one client
    messages=[{"role": "user", "content": "hi"}],
)
```

`llmux.start()` resolves the binary (`$LLMUX_BINARY` → bundled `bin/llmux` →
`llmux` on `PATH`), picks a free loopback port, launches it with
`LLMUX_ADDR=127.0.0.1:<port>` and the environment inherited (so provider keys
pass through), polls `/health`, and terminates the child at exit. Start is lazy,
singleton and thread-safe.

Without the `openai` package, `llmux.base_url()` / `llmux.openai_base_url()`
give you a URL for `urllib`, `httpx`, `requests` — anything.

`examples/sidecar_chat.py` does the whole surface with `urllib` alone.

## Direct

```python
from llmux import Gateway

with Gateway() as gw:                        # config JSON, or None for env defaults
    gw.models()
    answer = gw.chat({"model": "gpt-4o-mini",
                      "messages": [{"role": "user", "content": "hi"}]})

    gw.stream({"model": "gpt-4o-mini", "messages": [...]},
              lambda chunk: print(chunk["choices"][0]["delta"].get("content", ""), end=""))

    for chunk in gw.stream_iter({"model": "gpt-4o-mini", "messages": [...]}):
        ...
```

**Always use the context manager.** `__exit__` closes the handle on every path
including an exception; a handle leaked from an error path stays open for the
life of the interpreter. Error strings are freed too, before the exception is
raised — `LLMuxError` carries a Python copy.

`ctypes` is the entire dependency list. No `cffi`, no compiled extension, no
build step in your environment.

### Streaming and threads

`llmux_stream` blocks and calls back once per chunk. **The callback runs on the
thread that called it**, synchronously, before `stream()` returns. That is
measured, not assumed: `ffi/ctest/smoke.c` compares `pthread_self()`, and
`tests/test_direct.py` compares `threading.get_ident()` from the main thread and
again from a worker thread. So there is no thread-attach dance to write here,
and none is written.

Two details that are easy to get wrong and are handled for you:

- `ctypes.CFUNCTYPE` (not `PYFUNCTYPE`) reacquires the GIL for the callback.
- An exception raised inside a ctypes callback would otherwise be printed to
  stderr while 0 is returned to Go — swallowed, and the stream carries on. The
  binding catches it, stops the stream, and re-raises it out of `stream()`.

Returning `False` from the callback stops the stream early. That is a normal
end, not a failure, in the ABI and here — **and it is a different thing from
cancelling.** `stream()` returns cleanly with no exception, because your
callback made the decision and llmux does not hand you back an error for your
own choice. It stops *this process* from receiving any more chunks. Whether it
also stops the *provider* from generating them is not this binding's call —
that is `llmux_cancel`'s job, and the next section measures the difference.

`stream_iter()` is the generator form. It costs a worker thread, and that cost
is inherent — `llmux_stream` pushes, a generator pulls, and nothing bridges push
to pull without a second stack. `stream()` is the honest shape of the ABI;
`stream_iter()` is the shape Python code wants. Both are supported.

### Cancellation

```python
for chunk in gw.stream_iter({"model": "gpt-4o-mini", "messages": [...]}):
    ...
    break          # -> GeneratorExit -> gw.cancel() -> llmux_cancel
```

`break`, an explicit `gw.stream_iter(...).close()`, or letting the generator be
garbage collected all reach `Gateway.cancel()`, which binds `llmux_cancel` — the
seventh function in the ABI, added in llmux 0.1.5 alongside `llmux_abi_version`,
`llmux_new`, `llmux_close`, `llmux_call`, `llmux_free`, and `llmux_stream`. This
matters because it used to not be true, and the difference is a real amount of
money:

**Measured** against `sdks/fake-upstream.py` serving ten words at 100 ms per
chunk (`python3 ../fake-upstream.py --chunk-delay-ms 100 --text "one two three
four five six seven eight nine ten"`, `sdks/python` as the working directory,
Python 3.13.9, abi 0.1.5): a consumer that broke out of `stream_iter()` after 3
chunks left the upstream at exactly **3 of 12** — 10 words plus a trailing
finish frame plus a usage frame — per `GET /generated`. Before `cancel()` was
wired into abandonment, this method only set a local flag its callback checked
before forwarding the *next* chunk to Python: that stops chunks from reaching
your code, but by itself it closes nothing, so at least one more chunk than the
consumer asked for gets generated (and metered) before the callback for it can
even run and reject it — a real, measured gap this fix removes, not a
theoretical one. `examples/direct_chat.py`, run through `run-demo.sh`, prints
the fixed measurement on every run:

```
cancel       stream_iter stopped the consumer at 3; the upstream generated 3 of 12
```

`Gateway.cancel()` is also the low-level primitive if you are not using
`stream_iter()`:

```python
gw.cancel()   # abort every call in flight on this handle; the handle stays open
```

It is safe from another thread while `call`/`stream`/`stream_iter` blocks on
this handle — `ctypes.CDLL` functions release the GIL for the duration of the
native call, which is what lets a second thread run `cancel()` while the first
is parked inside `llmux_stream`; measured in
`tests/test_direct.py::Cancellation::test_cancel_from_another_thread_unblocks_a_blocked_stream`.
It is also safe to call *from inside the chunk callback that is currently
running* — unlike `close()`, which must never be called from a callback
because it waits (up to a few seconds) for that very call to return. A
cancelled call does not end quietly like a callback returning `False`: it
raises `LLMuxError("context canceled")`, because tokens already served are
still metered and you should not mistake this for the normal end your own
callback chose. It is a no-op if nothing is running, if the handle is unknown,
or if it has already been closed, and the handle itself survives a cancel — a
fresh call on it afterwards runs clean.

**`cancel()` is per-handle, not per-call.** It aborts *every* call in flight on
that `Gateway`, not only the stream you were thinking of. Two `stream_iter()`
loops sharing one `Gateway` and abandoning one cancels both. If you need to
cancel streams independently, give each its own `Gateway` — construction is
inert (see above), so this costs a handle, not a connection.

## The costs of direct mode — read these

1. **The Go runtime lives in your interpreter.** Its garbage collector, its
   scheduler, and its signal handlers. Measured: Go replaces five — `SIGSEGV`,
   `SIGBUS`, `SIGFPE`, `SIGPIPE`, `SIGURG` — and leaves three more in place with
   `SA_ONSTACK` added (`SIGILL`, `SIGXFSZ`, `SIGUSR2`). A crash reporter or a
   sanitizer build with its own `SIGSEGV` handling can conflict; Go chains to a
   pre-existing handler in most cases, and "most" is the honest word.

   **Profiling is not affected.** `SIGPROF` is not touched, so `yappi` and
   anything else driven by `setitimer(ITIMER_PROF)` keeps working with the
   library loaded; `py-spy` samples from outside the process and was never at
   risk either way. The measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).

2. **It is not fork-safe.** After `fork()` without `exec()` the Go runtime in
   the child is broken. Concrete victims in Python:

   | victim | why | fix |
   |---|---|---|
   | `multiprocessing`, `ProcessPoolExecutor` | `fork` is the default start method on Linux before 3.14 | `multiprocessing.set_start_method("spawn")` |
   | uWSGI | master imports the app, then forks workers | load the library in `@postfork`, or use the sidecar |
   | Gunicorn sync / gthread workers | same pre-fork model | load in `post_fork`, or use the sidecar |
   | Celery prefork pool | same | `worker_process_init`, or use the sidecar |

   **`examples/fork_hazard.py` demonstrates it.** Measured on darwin/arm64,
   Python 3.13.9:

   ```
   after os.fork(), in the child:
     models  (memory only)              completed  (0.00s)
     chat    (needs the netpoller)      HUNG — no result in 5.0s, SIGKILLed  (5.00s)
     new + chat  (a fresh gateway)      HUNG — no result in 5.0s, SIGKILLed  (5.01s)

   through multiprocessing:
     start_method=fork     'HUNG — no result in 5.0s, killed'               (5.00s)
     start_method=spawn    'one two three four'                             (0.07s)
   ```

   Read the first line twice. **`models` succeeded.** It is answered from memory
   on the calling goroutine, so it does not need the threads that did not come
   across the fork — and a health check that only lists models therefore reports
   a clean bill of health for a process that will hang on the first chat
   completion. "It worked when I tried it" is not evidence here.

3. **The shared library is 12–17 MB.** Measured: 12,823,104 bytes on
   darwin/arm64, 17,356,264 bytes on linux/arm64.

4. **Prebuilt libraries exist for two platforms.** For **llmux**:

   | target | status |
   |---|---|
   | darwin/arm64 | built and smoke-tested (40/40 C checks) |
   | linux/arm64 | built and smoke-tested in a `golang:1.25` container |
   | linux/amd64 | built in CI only; not produced on a development machine |
   | windows/amd64 | **not built. No DLL exists.** |
   | darwin/amd64 | **not built.** |

   There is no Windows wheel with a DLL in it, and this page will not imply
   there is. On those platforms, use the sidecar.

5. **Latency is not the reason to embed.** Measured on darwin/arm64: the
   boundary itself is ~4 µs in-process against ~46 µs over loopback HTTP, and a
   `chat` call including the upstream round trip is ~80–92 µs against
   ~102–109 µs. Against a model that takes hundreds of milliseconds to answer,
   40 µs is noise. Embed for *no second process, no port, no loopback surface* —
   not for speed.

6. **Two Go libraries mean two Go runtimes.** Loading `libllmux` and another
   Go-built c-shared library in one process gives you two independent runtimes
   with two GCs. It works; it is not free.

## Examples

```bash
examples/run-demo.sh                    # all three, end to end
examples/run-demo.sh direct_chat.py     # just one
```

`run-demo.sh` starts `ffi/fakeupstream` — the same OpenAI-compatible fake the Go
tests, the C smoke test and the latency benchmark use — so every example runs
with no provider account and no network. It also starts a second upstream,
`sdks/fake-upstream.py`, at 100 ms per chunk, purely so `direct_chat.py` can
measure a cancellation against something slow enough to interrupt. It builds
the library and the binary if they are missing (needs a Go toolchain for that;
`fake-upstream.py` itself is stdlib Python and needs no build step).

| file | mode | shows |
|---|---|---|
| `examples/direct_chat.py` | direct | version probe, context manager, `models`/`chat`, streaming both ways, cancellation (measured against the upstream's own count), the error path, use-after-close |
| `examples/sidecar_chat.py` | sidecar | spawn and manage, `urllib` only, SSE streaming, HTTP errors, the optional `openai` client |
| `examples/fork_hazard.py` | direct | the fork failure, reproduced with a watchdog, and `spawn` fixing it |

## Tests

```bash
cd sdks/python && python3 -m unittest discover -s tests
```

`tests/test_sidecar.py` drives a fake `llmux` fixture; `tests/test_direct.py`
drives the real shared library against fake upstreams and skips cleanly when no
library is resolvable. The direct tests are about the five things a ctypes
binding gets wrong, not about llmux:

- **ownership** — `llmux_call` returns malloc'd memory that only `llmux_free`
  may release, so a `c_char_p` restype would leak every result. One test counts
  the frees and asserts each pointer is freed exactly once.
- **handles on the error path** — closed by the context manager after an
  exception.
- **the callback contract** — which thread it runs on (from the main thread and
  from a worker), and that an exception inside it propagates.
- **abort** — returning `False` stops the stream and is not an error.
- **cancellation** — `Gateway.cancel()` unblocks a stream from another thread
  and from inside its own callback, the handle survives it, and abandoning
  `stream_iter()` actually stops the upstream instead of only the local
  delivery of chunks. This class alone talks to `sdks/fake-upstream.py`
  instead of the in-process `tests/fake_openai.py` fixture the rest of the file
  uses, because answering "did the provider actually stop" needs a provider
  slow enough to interrupt and honest enough to count what it wrote.

## Reference

```
llmux.start(port=None, config=None, env=None, timeout=10.0) -> str
llmux.base_url() -> str
llmux.openai_base_url() -> str
llmux.stop() -> None
llmux.OpenAI(api_key="llmux-local", **kw)        # needs the `openai` package
llmux.AsyncOpenAI(api_key="llmux-local", **kw)

llmux.library_path(path=None) -> str
llmux.load_library(path=None, require_version=None) -> ctypes.CDLL
llmux.abi_version(lib=None) -> str
llmux.Gateway(config=None, *, library=None, require_version=None)
    .handle / .closed / .close() / context manager
    .cancel()                                     # abort in-flight calls; handle stays open
    .call(method, request=None) / .chat(req) / .embed(req) / .models()
    .stream(req, on_chunk, *, raw=False)
    .stream_iter(req, *, raw=False, buffer=64)     # abandonment -> cancel()
llmux.LLMuxError, llmux.LLMuxLibraryError
```

Probe the version at startup if you load the library from a path you do not
control: `llmux.load_library(require_version="0.1.2")` turns a stale `libllmux`
earlier on the load path into a clear error instead of behaviour that looks like
an llmux bug.

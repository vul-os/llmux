# llmux for Rust

Two modes, both supported, both with a runnable example in [`examples/`](examples).

| | what it is | the crate |
| --- | --- | --- |
| **Direct** | `dlopen`s `libllmux` and runs the gateway **inside your process** | [`llmux::direct`](src/direct.rs) |
| **Sidecar** | spawns and supervises `llmux serve`, talks HTTP | the crate root, plus [`llmux::http`](src/http.rs) |

**For most Rust programs, direct is the better default** — and Rust is one of
the few languages where that is true without qualification. The two costs that
push Python, Ruby and the JVM toward the sidecar (a runtime that pre-forks, and
a runtime with its own opinions about signals) do not apply to Rust. Read
[the costs](#the-costs) anyway, then pick.

## Run the examples

Offline, with no provider keys and no network, against the OpenAI-compatible
fake in `ffi/fakeupstream`:

```
./sdks/rust/examples/run.sh            # both
./sdks/rust/examples/run.sh direct
./sdks/rust/examples/run.sh sidecar
```

Real output — darwin/arm64 (Apple silicon), rustc 1.97.1, Go 1.25.12, captured
against llmux 0.1.5, whose `libllmux.dylib` was 12,823,104 bytes:

```
==> direct (in-process, C ABI)
library: /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi:     0.1.5
models:  1580 bytes in 51.834µs
chat:    11.055584ms
refusal: llmux: "stream": true is not valid for llmux_call; use llmux_stream
stream:  one two three four
         6 chunks, first at 1.074125ms, total 1.365ms
early:   stopped after 2 chunk(s), Ok(()) — stopping is not a failure
bogus:   llmux: unknown method "no-such-method" (want one of: chat, embed, models)
cancel:  consumer saw 3 chunks
cancel:  handle survives — a fresh call on it still works
cancel:  GET /generated -> {"generated": 3, "streams": 1, "disconnects": 0}

==> sidecar (child process over HTTP)
sidecar: http://127.0.0.1:56401
openai:  http://127.0.0.1:56401/v1
models:  1581 bytes in 440.833µs
chat:    2.471209ms
stream:  one two three four
         6 chunks, first at 991.583µs, total 1.143208ms
bogus:   HTTP 404: {"error":{"message":"no route for model \"no-such-model-anywhere\" ...
```

The `early:` line and the `cancel:` block measure two different mechanisms and
are not in tension. `early:` is `stream_with`'s callback returning `false`,
which stops the *next* chunk from reaching the callback that just ran — it
says nothing about whether upstream had already started generating that next
chunk. `cancel:` is the new `llmux_cancel`, which `ChunkStream`'s `Drop` calls
*first*, before it touches anything else — it interrupts whatever chunk llmux
is already blocked reading from upstream, which a callback's return value has
no way to do. See [Cancellation](#cancellation) for the number that proves the
difference: the `cancel` demo above stops upstream generation at exactly the
3 chunks the consumer took, out of 12 a full run would have produced.

Those `models` timings are single cold calls from an example, not a benchmark;
`ffi/bench` is the measurement (~4 µs in-process against ~46 µs loopback, over
1000 requests). Do not quote the example's numbers.

## Direct

```rust
use llmux::direct::Gateway;

let gw = Gateway::open(None)?;                  // None = defaults + environment
let models = gw.call("models", None)?;          // JSON in, JSON out
let chat = gw.call("chat", Some(request_json))?;

for chunk in gw.stream(streaming_request_json)? {
    print!("{}", chunk?);
}
# Ok::<(), llmux::direct::Error>(())
```

**Handles close on `Drop`.** `Gateway` owns the `u64` and releases it when it
goes out of scope — on the happy path, on every `?`, and on a panic unwind.
There is no `close()` to forget.

**Every returned string is freed.** Results and error messages alike go back
through `llmux_free` and nothing else. `Error::from_c` copies the message into a
`String` and frees the original *before* constructing the `Error`, which is the
step a hand-written binding usually misses.

**Errors are `Result`,** with the library's own message in `Error::Llmux`. That
message is plain UTF-8 text and deliberately not JSON — do not parse it.

**Streaming is an `Iterator`.** `llmux_stream` is a blocking call with a
callback; an iterator is a pull API. `Gateway::stream` bridges them with one
worker thread and a **rendezvous channel** (`sync_channel(0)`), so nothing is
buffered: the worker blocks inside the callback until you take the chunk.
Time-to-first-token and backpressure are both real. **Dropping the iterator
early calls `llmux_cancel`** — a `break`, an early `?`, a panic — before it
touches the channel or joins the worker; see [Cancellation](#cancellation) for
why the ordering is the whole point. If you want the callback form without the
thread, `Gateway::stream_with` is right there.

**Panics do not cross into Go.** The callback trampoline wraps your closure in
`catch_unwind`; a panic stops the stream instead of unwinding through a Go call
frame, which would be undefined behaviour.

### Cancellation

llmux 0.1.5 added the ABI's seventh symbol, `llmux_cancel`. Before it, the
only way out of a blocked `llmux_call` or `llmux_stream` was `llmux_close`,
which destroys the gateway and every other call running on it —
`llmux_cancel` aborts what is in flight and leaves the handle open.

```rust
use llmux::direct::Gateway;

let gw = Gateway::open(None)?;

// The idiomatic path: abandon the iterator and llmux_cancel happens for you.
for chunk in gw.stream(streaming_request_json)? {
    print!("{}", chunk?);
    break; // ChunkStream::drop calls llmux_cancel before it does anything else
}

// Detached, for a thread that does not hold the iterator at all — a `ctrlc`
// handler, a `tokio::select!` arm, a watchdog thread.
let cancel = gw.cancel_handle(); // Clone + Send + Sync + 'static
std::thread::spawn(move || {
    std::thread::sleep(std::time::Duration::from_secs(5));
    cancel.cancel();
});

// The low-level primitive both of the above call underneath.
gw.cancel();
# Ok::<(), llmux::direct::Error>(())
```

**The measured number.** Against `sdks/fake-upstream.py` — the harness built
specifically to answer this, since it counts a chunk as generated only once it
is written to a socket and serves that count at `GET /generated` — streaming a
10-word answer at 100ms/chunk and taking exactly 3 chunks before abandoning the
iterator: **the consumer saw 3 chunks; the upstream generated 3 of the 12** a
full run produces (10 word-chunks plus a `finish_reason` frame plus a `usage`
frame). Reproduced with `cargo test --test direct
dropping_the_iterator_early_reaches_llmux_cancel -- --nocapture` and with
`examples/run.sh direct` (see the `cancel:` lines above). Without the `Drop`
calling `llmux_cancel` first, this number is 4, not 3: the chunk after the
last one delivered is already being read from upstream by the time the
consumer's early exit would otherwise be noticed (see `ChunkStream`'s `Drop`
for the full mechanism).

**Never surfaced as an error.** The raw ABI call a cancelled `call` or
`stream_with` was blocked in returns `Err` with the message `"context
canceled"` (check it with `Error::is_cancelled` rather than string-matching it
yourself). `ChunkStream` never hands you that `Err` — its own cancel on
`Drop` is moot (nobody is listening any more), and a `CancelHandle` cancelling
a stream still being read from another thread just ends the iteration, the
same as if the upstream had finished. Abandoning a stream is not a failure a
Rust consumer asked to see.

**The handle survives.** A cancelled gateway is still open: `gw.call(...)`
after a cancel, or a fresh `gw.stream(...)`, both work normally on the same
handle. Cancelling twice, or cancelling a handle with nothing running, is a
documented no-op.

**Per handle, not per call — this is the one caveat to internalize.**
`llmux_cancel` aborts *every* call currently in flight on the gateway, not
just the stream you had in mind. Two concurrent streams on one `Gateway`
cannot be cancelled independently; cancelling one cancels both. If your
program needs independent cancellation scopes, give each scope its own
`Gateway::open` — a gateway is cheap to open relative to a model call, and
there is no other way to get this isolation.

### The library is loaded once and never unloaded

This is worth knowing before you write your own binding, because it is a bug we
shipped and then found:

> An early version of this module owned the `libloading::Library` per-gateway
> and let it drop. A test that opened and closed 200 gateways in a loop **hung** —
> each iteration was a fresh `dlopen` of a Go `c-shared` object followed by a
> `dlclose`, each cycle took longer than the last, and the process had to be
> killed.

`libllmux` starts the Go runtime and its threads, and Go has no "shut the
runtime down" entry point, so `dlclose` would unmap code that threads are still
executing. `Api::shared` therefore caches one `&'static Api` per library path
for the life of the process and leaks the mapping on purpose.
`tests/direct.rs::many_open_close_cycles_do_not_exhaust_the_registry` is the
regression test; it takes ~20 ms now.

### Finding the library

1. `$LLMUX_LIBRARY` — an explicit path always wins.
2. `dist/ffi/<goos>_<goarch>/libllmux.{dylib,so}`, walking up from the crate.
3. The bare file name, handed to the platform loader.

Probe the version at startup and refuse a mismatch, because a shared library
resolves off a load path you may not control:

```rust
let gw = llmux::direct::Gateway::open_checked("0.1.2", None)?;
# Ok::<(), llmux::direct::Error>(())
```

## Sidecar

```rust
let base = llmux::start(llmux::Options::default())?;   // idempotent
let body = llmux::http::post_json(&format!("{base}/v1/chat/completions"), req, timeout)?;
llmux::http::post_sse(&format!("{base}/v1/chat/completions"), req, timeout, |chunk| {
    print!("{chunk}");
    true
})?;
llmux::stop();
# Ok::<(), Box<dyn std::error::Error>>(())
```

Binary resolution: `LLMUX_BINARY`, then the bundled `bin/llmux`, then `PATH`.
For local development: `go build -o sdks/rust/bin/llmux ./cmd/llmux`.

With the `async-openai` feature, `llmux::openai_client()` returns a configured
`async_openai::Client`. Provider keys are inherited from the child's environment.

**Rust has no `defer`,** so a program that owns a child process has to stop it on
the error path explicitly. `examples/sidecar.rs` calls `llmux::stop()` from both
arms of its `main`, which is the closest thing to RAII available for a process
the crate owns as a `static` rather than a value you hold.

### Why there is no HTTP dependency

`llmux::http` is ~200 lines of `std::net` speaking HTTP/1.1 to `127.0.0.1`. It
is not a general HTTP client and must not be used as one: no TLS, no redirects,
no proxies, no connection reuse. The alternative was making every consumer of a
crate whose main job is to `dlopen` a shared library inherit `hyper`, `tokio`
and a TLS stack. If your program already depends on `reqwest`, use `reqwest` —
the wire contract is ordinary JSON over HTTP and nothing in this crate is
privileged.

## The costs

Applies to the **direct** mode only. The sidecar has none of these.

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   handlers for `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPIPE` and `SIGURG` (measured;
   `SIGPROF` is *not* touched). A Rust program that
   installs its own (a crash reporter, a sampling profiler, a sanitizer build)
   can conflict. Go chains to a pre-existing handler in most cases; "most" is the
   honest word.
2. **Not fork-safe.** After `fork()` without `exec()` the Go runtime in the child
   is broken. The concrete Rust victims: any direct `libc::fork()` in your own
   code, and any pre-fork worker model built on it. This is a much shorter list
   than Python's, because idiomatic Rust concurrency is threads and `tokio`, not
   forked workers, and `std::process::Command` always `exec`s. If you do fork,
   load the library **after** the fork, in the child.
3. **The shared library is 12–17 MB.**
4. **Prebuilt binaries exist only for darwin/arm64 and linux/arm64.**
   linux/amd64 is CI-only. **windows/amd64 and darwin/amd64 do not exist** — no
   `llmux.dll` has ever been built by anyone. `library_file_name()` names
   `llmux.dll` for completeness; that is not a promise one ships. If you need a
   platform outside that list, use the sidecar, which is a plain Go binary and
   cross-compiles anywhere.
5. **Latency is not the reason to embed.** Measured in `ffi/bench`: ~4 µs
   in-process against ~46 µs loopback for the boundary, ~80–92 µs against
   ~102–109 µs for a whole chat call. A real model answers in hundreds of
   milliseconds. The reasons are no second process to supervise, no port to
   bind, and no loopback surface to secure.
6. **The sidecar is not the recommended default for Rust,** unlike some
   languages. It is the right answer when you need per-tenant virtual keys and
   budgets (enforced by the HTTP shell's auth middleware, which an in-process
   caller sits inside of and bypasses by construction), when several processes
   should share one gateway and one cache, when you want llmux restartable
   independently of your program, or when you are on a platform with no prebuilt
   library.

## Would gitstate be better off with direct mode?

`gitstate` is the real in-suite Rust consumer of llmux, and this SDK does not
touch it. The question was asked, so here is the answer with the evidence.

**No. HTTP remains the better fit for gitstate.** Three reasons, in order of
weight:

1. **llmux is not gitstate's dependency — an OpenAI-compatible endpoint is.**
   `crates/gitstate-classify/src/llm.rs` is documented as speaking to "any
   OpenAI-compatible chat endpoint (llmux, a local model, OpenAI itself)", and
   it is configured with `VULOS_LLMUX_URL` **or** `OPENAI_BASE_URL`. Linking
   `libllmux` would convert a swappable endpoint into a hard dependency on one
   implementation, and would do it in the crate whose whole point is that the
   user chooses where inference happens.
2. **gitstate ships a Tauri desktop app.** Direct mode has no prebuilt library
   for windows/amd64 or darwin/amd64, and a desktop app that only runs on Apple
   silicon and arm64 Linux is not a desktop app. It would also add 12–17 MB per
   platform to the bundle.
3. **The call rate makes the boundary irrelevant.** `classify_items` and
   `effort_items` are caller-triggered, batch every selected item into one
   prompt, and make **one** round trip. Saving ~40 µs of transport on a request
   that waits on a model for seconds is not a saving.

There is a fourth point that cuts the other way and is worth being straight
about: gitstate's seam is at the **domain** level (`Classifier`), not the
transport level, so there is no `HttpTransport` to swap. Adopting direct mode
would mean a new `Classifier` impl next to `LlmClassifier`, not a config change
— and `gitstate-report` and two daemon ops already reach around the trait to the
concrete `LlmClassifier`, so "just add an impl" is more work than it sounds.
That is an argument against churn, not an argument for HTTP, and it is the
weakest of the four.

**Where direct mode would suit gitstate:** the daemon (`gitstated`) on a Linux
arm64 server, if it ever wanted a gateway with no second process to supervise.
That is a deployment gitstate does not currently have.

## Layout

```
sdks/rust/
  src/lib.rs       the sidecar launcher (spawn, health, stop)
  src/direct.rs    the C ABI binding — Gateway, Drop, Result, ChunkStream,
                    CancelHandle
  src/http.rs      a std-only HTTP/1.1 client for the sidecar (POST JSON, SSE)
  examples/direct.rs
  examples/sidecar.rs
  examples/run.sh  runs both offline against ffi/fakeupstream
  tests/direct.rs  gated on a real libllmux; skips loudly when there is none
  tests/sidecar.rs
```

`cargo test` runs 32 tests. The `tests/direct.rs` ones are gated on a built
library and **say which way they went** rather than reporting a silent pass —
run with `--nocapture` to see `direct tests RAN` or `direct tests SKIPPED`.
Two of them additionally spawn `sdks/fake-upstream.py` and skip (loudly) if
`python3` is not on PATH: `dropping_the_iterator_early_reaches_llmux_cancel`
and `cancel_handle_from_another_thread_ends_the_iterator_without_an_error`,
the cancellation tests.

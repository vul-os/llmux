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
against llmux 0.1.2, whose `libllmux.dylib` was 12,769,346 bytes (today's
darwin/arm64 build is 12,787,504):

```
==> direct (in-process, C ABI)
library: /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi:     0.1.2
models:  1580 bytes in 74.958µs
chat:    1.594875ms
refusal: llmux: "stream": true is not valid for llmux_call; use llmux_stream
stream:  one two three four
         6 chunks, first at 575.583µs, total 806.916µs
early:   stopped after 2 chunk(s), Ok(()) — stopping is not a failure
bogus:   llmux: unknown method "no-such-method" (want one of: chat, embed, models)

==> sidecar (child process over HTTP)
sidecar: http://127.0.0.1:63360
openai:  http://127.0.0.1:63360/v1
models:  1581 bytes in 359.542µs
chat:    923.083µs
stream:  one two three four
         6 chunks, first at 504µs, total 617.125µs
bogus:   HTTP 404: {"error":{"message":"no route for model \"no-such-model-anywhere\" ...
```

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
Time-to-first-token and backpressure are both real. Dropping the iterator early
closes the channel, which makes the callback return non-zero, which stops the
stream — and `Drop` joins the worker, so no thread is orphaned. If you want the
callback form without the thread, `Gateway::stream_with` is right there.

**Panics do not cross into Go.** The callback trampoline wraps your closure in
`catch_unwind`; a panic stops the stream instead of unwinding through a Go call
frame, which would be undefined behaviour.

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
  src/direct.rs    the C ABI binding — Gateway, Drop, Result, ChunkStream
  src/http.rs      a std-only HTTP/1.1 client for the sidecar (POST JSON, SSE)
  examples/direct.rs
  examples/sidecar.rs
  examples/run.sh  runs both offline against ffi/fakeupstream
  tests/direct.rs  gated on a real libllmux; skips loudly when there is none
  tests/sidecar.rs
```

`cargo test` runs 27 tests. The `tests/direct.rs` ones are gated on a built
library and **say which way they went** rather than reporting a silent pass —
run with `--nocapture` to see `direct tests RAN` or `direct tests SKIPPED`.

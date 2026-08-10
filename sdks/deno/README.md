# llmux for Deno

Two ways to use llmux from Deno, both supported, both with a runnable example
in [`examples/`](examples). One module, [`mod.ts`](mod.ts), exports both.

| | export | what it is | streaming | event loop |
|---|---|---|---|---|
| **Direct** | `Gateway` | libllmux loaded into this process over the C ABI | `for await` async iterator | free |
| **Sidecar** | `Sidecar` | `llmux serve` in a child process, HTTP over loopback | SSE, `for await` | free |

## Which one should I use?

**Deno is the one JS runtime where direct mode is not a compromise**, so this
page does not steer you away from it the way [`../node/README.md`](../node/README.md)
has to. A symbol declared `nonblocking: true` runs on Deno's blocking-task pool
and `Deno.UnsafeCallback` marshals the chunk callback back into the isolate, so
streaming really is an async iterator, the event loop really does keep running,
and the process really does exit afterwards. Measured below.

Choose the **sidecar** anyway if you need per-tenant virtual keys, budgets or
model allow-lists — those are enforced by the HTTP shell's auth middleware and
there is deliberately no authentication on the C ABI, because an in-process host
is already inside the trust boundary. Choose it too if you are on
windows/amd64 or darwin/amd64, where no shared library exists at all.

---

## Permissions and versions

Tested on **Deno 2.7.11 (aarch64-apple-darwin)**.

| mode | flags |
|---|---|
| direct | `--allow-ffi` (plus `--allow-env=LLMUX_LIB` if you point it with that variable) |
| direct, the example | `--allow-ffi --allow-run --allow-env --allow-read --allow-net` — run/read/env are for the fake upstream it spawns, not for llmux; net is for THIS process's own `fetch()` of the fake upstream's `GET /generated`, used to prove cancellation actually stopped it |
| sidecar | `--allow-run --allow-net --allow-env` (`--allow-read --allow-write` in the example, for its temporary config file) |

**`--unstable-ffi`**: not needed here. On Deno 2 the FFI API is stable and
`--allow-ffi` alone works for direct mode itself — verified by running
`examples/direct.ts` with exactly
`--allow-ffi --allow-run --allow-env --allow-read --allow-net` and nothing
else (the last one is for the example's own fetch, not for `Deno.dlopen`). On
Deno 1.x, `Deno.dlopen` was unstable and you also needed `--unstable-ffi`
(or plain `--unstable` before 1.30). Passing it on 2.7.11 is accepted and does
nothing.

`resolveLibrary()` reads `LLMUX_LIB` only if env permission is already granted,
checked with `Deno.permissions.querySync` — so running with `--allow-ffi` alone
does not trip a permission prompt or a throw; it just falls through to the
checkout path.

---

## Direct

```ts
import { abiVersion, Gateway } from "./mod.ts";

using gw = Gateway.open({ expectVersion: abiVersion() });

const answer = await gw.call("chat", {
  model: "gpt-4o-mini",
  messages: [{ role: "user", content: "hi" }],
});

for await (const chunk of gw.stream({ model: "gpt-4o-mini", messages: [{ role: "user", content: "hi" }] })) {
  Deno.stdout.writeSync(new TextEncoder().encode(chunk.choices?.[0]?.delta?.content ?? ""));
}
```

`Gateway.open` is inert: no goroutines, no sockets (unless your config names a
Postgres DSN), no price sync, no spend flusher. Nothing happens until you call.

`Gateway` implements `Symbol.dispose`, so `using` closes the handle on every
exit path out of the block, throw included. `close()` is idempotent, as
`llmux_close` is.

- `gw.call(method, request)` — off the event loop, returns a promise.
- `gw.callSync(method, request)` — same call on this thread, blocking. Useful in
  a script; it is the one entry point here that can stall the isolate.
- `gw.stream(request, options?)` — an async generator. `break`, `return`, an
  exception in the loop body, or `options.signal` firing all stop the stream —
  see "Cancellation" below.
- `gw.cancel()` — the low-level escape hatch `stream()`'s `signal` is built on.
  Aborts every call in flight on this handle and leaves it open. See
  "Cancellation".

Everything the library allocates is released: `takeString` frees each result
with `llmux_free` before returning the string, `takeError` does the same for the
error message, and the `UnsafeCallback` is closed only **after** the
`llmux_stream` promise has settled — closing it while the library still holds
the pointer is how this becomes a segfault instead of an exception. The
generator's `finally` runs on `break` and on `throw` as well as on completion,
so the stop-and-wait sequence is not something the caller has to remember.

### Cancellation

llmux v0.1.5 added a seventh ABI symbol, `llmux_cancel`
(`ffi/include/llmux.h`). Before it, the only way out of a call blocked in
`llmux_call` or `llmux_stream` was `llmux_close`, which destroys the gateway
and every other stream running on it. `llmux_cancel` aborts what is running
and leaves the handle open — `llmux_call(h, "models", ...)` right after a
cancel succeeds normally.

```ts
// The idiomatic way: an AbortSignal, fired from wherever your cancel
// decision actually lives — a timeout, a sibling request, a UI button —
// not necessarily the code that is reading chunks.
const controller = new AbortController();
setTimeout(() => controller.abort(), 5000);
for await (
  const chunk of gw.stream({ model, messages }, { signal: controller.signal })
) {
  // ...
}

// The low-level escape hatch, if you are not holding a stream's iterator at
// all — e.g. cancelling from a signal handler or a supervisor task.
gw.cancel();
```

**`llmux_cancel` is per-HANDLE, not per-stream or per-call.** It aborts EVERY
call in flight on the gateway it is given — every `stream()`, every `call()`
and `callSync()` currently running. `stream()`'s `signal` option does not
change that: aborting the signal you gave to `streamA` also aborts `streamB`
if both are running on the same `Gateway`. **If you need independent
cancellation for concurrent streams, give each one its own gateway** —
`Gateway.open()` is inert and cheap enough to call per cancellation scope; do
not share one handle across scopes and expect only one of them to stop.

Two different outcomes depending on who decided to stop, both exercised in
`examples/direct.ts`:

- **The stream's own consumer stops it** — `break`, `return`, an exception in
  the loop body, or the `signal` given to THIS `stream()` call firing — and
  nothing is thrown. `llmux_stream` returns an error internally (`rc=-1`, the
  message is exactly `context canceled` — see `llmux_cancel` in the header),
  but that is your own decision coming back to you, not a failure to hand you,
  the same principle already applied to a callback returning non-zero.
- **Something else cancels the gateway out from under this stream** —
  `gw.cancel()` called from unrelated code, or another stream's `signal`, or
  `close()` — and this stream's `for await` throws. Swallowing that too would
  make an externally-forced stop look identical to the stream finishing on its
  own, which is exactly the confusion the per-handle caveat above is trying to
  prevent.

An already-aborted `AbortSignal` rejects before any native call is made at
all — no `llmux_stream`, no allocation, nothing to unwind. See "What
cancelling actually stops" below for the measured numbers.

### Run it

```
../../scripts/build-ffi.sh
deno task example:direct
```

```
deno       2.7.11 on darwin/aarch64
library    /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi        0.1.5

handle     1

models       openai/gpt-4o-mini, google/gemini-1.5-flash, deepseek/deepseek-chat, anthropic/claude-3-5-sonnet, anthropic/claude-3-5-haiku, google/gemini-1.5-pro, openai/gpt-4o
chat         "the quick brown fox jumps over the lazy dog"
             took 379 ms; the event loop ticked 155x meanwhile

stream      the quick brown fox jumps over the lazy dog
             10 chunks, event loop ticked 160x during the stream

break       consumed 3 chunks; the C callback fired 3x (10 = the whole answer)
            no error raised — stopping is your decision, not a failure

cancel      consumer saw 3 chunks before AbortSignal fired
            AbortError raised on the chunk it was awaiting when the signal fired
            upstream generated 3 of 10 words total

error       no route for model "no-such-model" (providers: fake)
stream:true llmux: "stream": true is not valid for llmux_call; use llmux_stream
closed      llmux gateway is closed
```

Those tick counts are the whole argument: a 1 ms timer fires 155× across a
379 ms call. The same measurement on Node is `0`.

No API key and no network beyond loopback — the example spawns
[`examples/fake-upstream.mjs`](examples/fake-upstream.mjs), an
OpenAI-compatible fixture that prints the llmux config routing `demo` at
itself, and the cancellation demo also `fetch()`es that same fixture's own
`GET /generated` to see what the provider actually produced.

### Threads, honestly

The header says the chunk callback runs on the thread that called
`llmux_stream`, synchronously, and `ffi/ctest/smoke.c` asserts it with
`pthread_self()`. With `nonblocking: true` that thread is one of Deno's
blocking-task threads rather than the isolate's, and Deno queues the callback
into the isolate for you. That is Deno's machinery, not defensive code in this
binding — there is no thread-attach dance here, because there is nothing to
attach.

---

## Sidecar

```ts
import { Sidecar } from "./mod.ts";

await using side = await Sidecar.start();
// side.baseURL        -> http://127.0.0.1:<port>
// side.openaiBaseURL  -> http://127.0.0.1:<port>/v1
```

The same contract every llmux SDK implements: resolve the binary
(`options.binary` → `LLMUX_BINARY` → `llmux` on `PATH`), pick a free `127.0.0.1`
port, launch with `LLMUX_ADDR=127.0.0.1:<port>` inheriting the environment so
provider keys pass through, poll `/health` until it answers 200. `Sidecar`
implements `Symbol.asyncDispose`, so `await using` kills the child on the way
out of the block.

Two behavioural differences from direct mode:

- The child inherits stdio, so llmux's logs land on your stderr, interleaved
  with your own output.
- `llmux serve` **syncs a price catalog over the network at startup**. The
  library does not — a shared library loaded into someone else's process must
  not start background traffic they did not ask for.

### Run it

```
GOFLAGS=-mod=mod go build -o /tmp/llmux ../../cmd/llmux
LLMUX_BINARY=/tmp/llmux deno task example:sidecar
```

```
deno       2.7.11 on darwin/aarch64
sidecar    http://127.0.0.1:56310
openai     http://127.0.0.1:56310/v1

chat         "the quick brown fox jumps over the lazy dog"

stream      the quick brown fox jumps over the lazy dog
             10 chunks

error       HTTP 404 {"error":{"message":"no route for model \"no-such-model\" (providers: fake)","type":"invalid_request_error","c
```

(llmux's own log lines are interleaved with that on a real run — including
`pricing source failed` when the machine is offline — and are cut here.)

---

## What cancelling actually stops

A wrapper that turns a native callback into an async iterator has a failure mode
that looks like success: the consumer stops early, the loop exits, everything
looks cancelled — and the library ran to completion anyway, generating and
billing tokens nobody read. It was found in two other bindings in this suite, so
it is measured here rather than assumed.

Before llmux v0.1.5 there was no way to interrupt the blocked native call
itself: abandoning the iterator only ever set a local flag the C callback
checked at its *next* invocation, up to one whole provider chunk-delay away.
`llmux_cancel` closes that gap — abandoning `gw.stream()`'s iterator now calls
it from the generator's `finally`, which interrupts the network read the
native call is blocked in right now, instead of waiting for that read to
finish producing a chunk nobody wanted.

**The proof that matters is outside this process**, because a consumer can only
ever report what it itself saw, never what the provider went on generating and
billing after it stopped looking. `examples/direct.ts` streams from
[`examples/fake-upstream.mjs`](examples/fake-upstream.mjs) at 100 ms/chunk,
cancels through an `AbortSignal` fired from *outside* the consuming loop after
3 chunks — the way a real caller would use it, not a `break` inside the loop
that decides to stop itself — and then asks the fixture's own `GET /generated`
how many chunks it actually wrote to a socket. Measured on Deno 2.7.11,
darwin/arm64:

```
cancel      consumer saw 3 chunks before AbortSignal fired
            AbortError raised on the chunk it was awaiting when the signal fired
            upstream generated 3 of 10 words total
```

Three delivered, three generated, nothing extra billed.

`gw.stream(...)` also exposes `nativeChunks` — how many times the C callback
fired in this process, independent of what the upstream did. Breaking the same
kind of stream from inside its own loop tells the same story, with one honest
exception. Measured on Deno 2.7.11, darwin/arm64, breaking after 3 chunks of 10:

| upstream pace | consumed | C callback fired |
|---|---|---|
| 40 ms/chunk | 3 | **3** |
| as fast as the socket allows (`FAKE_DELAY_MS=0`) | 3 | **4** |

The flood row is not a regression — it is the one case the fix cannot reach,
and it is reported rather than assumed away. With the whole answer already
sitting in the socket's receive buffer, Go's HTTP client can read and dispatch
chunk 4 to the callback faster than this binding's `break` → generator
`finally` → `llmux_cancel` chain can run on the JS side: cancelling "as fast as
possible" is bounded by callback-marshalling latency, not network latency,
once there is no network latency left to interrupt. Any real model — token
generation is never instant — lands in the first row. One extra chunk is also
exactly what the old stop-flag-only implementation cost in every case, flood
included; this fix does not make that particular case worse, it just cannot
make it better.

Tokens already served are metered either way — cancelling stops the *next*
chunk, not the ones already delivered. See "Cancellation" above for which of
these outcomes raises an error and which does not.

---

## The costs of direct mode

Not footnotes. These are properties of `-buildmode=c-shared`;
[`ffi/README.md`](../../ffi/README.md) is the long version.

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   signal handlers. Measured: Go replaces five — `SIGSEGV`, `SIGBUS`, `SIGFPE`,
   `SIGPIPE`, `SIGURG` — and leaves three more in place with `SA_ONSTACK` added
   (`SIGILL`, `SIGXFSZ`, `SIGUSR2`). Go chains to a pre-existing handler in most
   cases; "most" is the honest word. Deno's V8 also installs signal handling, and
   a crash reporter attached to the process is a third party in that negotiation.

   **`SIGPROF` is not touched**, so sampling profilers that use it are
   unaffected. The measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).

2. **It is not fork-safe.** After `fork()` without `exec()` the Go runtime in
   the child is broken. Deno has no `fork()` in its API — `Deno.Command` always
   execs — so ordinary Deno code cannot hit this. It becomes real if you embed
   Deno (`deno_core`) inside a host that pre-forks, or run under a supervisor
   that forks after loading. The rule is unchanged: **load the library after the
   fork, in the worker, never in the master.**

3. **The library is 12–17 MB.** Measured: 12,823,104 bytes on darwin/arm64,
   17,356,264 bytes on linux/arm64.

4. **Prebuilt libraries exist for darwin/arm64 and linux/arm64 only.**
   linux/amd64 is built and tested in CI but not shipped from a developer
   machine. **windows/amd64 and darwin/amd64 do not exist — no `llmux.dll` has
   ever been produced by anyone.** Deno runs happily on Windows; direct mode
   does not. Use the sidecar there. (openrate's shared library has a *different*
   platform matrix — see `openrate/sdks/deno/README.md`. Do not read one as
   covering the other.)

5. **Latency is not the reason to embed.** Measured in `ffi/bench`: the boundary
   is ~4 µs in-process against ~46 µs over loopback, and a whole chat call is
   ~80–92 µs against ~102–109 µs. Against a model answering in hundreds of
   milliseconds that is noise. The reasons are no second process, no port and no
   loopback surface — not speed.

6. **Two Go libraries means two Go runtimes.** If this process also loads
   libopenrate you get two independent runtimes with two GCs. It works; it is
   not free.

---

## Layout and checks

```
sdks/deno/
  deno.json                   tasks, fmt and lint config
  mod.ts                      Gateway (direct) + Sidecar, no dependencies
  examples/direct.ts          direct mode, end to end
  examples/sidecar.ts         sidecar mode, end to end
  examples/fake-upstream.mjs  OpenAI-compatible fixture, so both run offline
```

`mod.ts` has **no third-party dependencies** — `Deno.dlopen`,
`Deno.UnsafeCallback` and `Deno.Command` are all in the runtime. The fixture is
plain JavaScript on purpose: the Node and Bun SDKs spawn the same file, and
`node:http` is the one HTTP server API all three runtimes implement.

```
deno task check      # deno check mod.ts and both examples
deno task lint
deno task fmt:check
```

# llmux for Bun

Two ways to use llmux from Bun, both supported, both with a runnable example in
[`examples/`](examples). One module, [`index.ts`](index.ts), exports both.

| | export | what it is | streaming | event loop |
|---|---|---|---|---|
| **Direct** | `Gateway` | libllmux loaded into this process over the C ABI | `for await` async iterator, on a Worker | **blocked during unary calls** |
| **Sidecar** | `Sidecar` | `llmux serve` in a child process, HTTP over loopback | SSE, `for await` | free |

## Which one should I use?

Direct mode on Bun is **half asynchronous**, and the halves are worth naming
before you choose:

- `gw.stream(...)` is a genuine async iterator and does **not** stall the event
  loop. It runs `llmux_stream` on a `node:worker_threads` Worker and posts each
  chunk back. Measured: 171 event-loop ticks of a 1 ms timer across a 10-chunk
  stream.
- `gw.call(...)` **does** stall it, for the entire upstream round trip.
  `bun:ffi` has no asynchronous call mode — there is no `nonblocking` option on
  a symbol the way Deno has one — so a unary call runs on the thread that made
  it. Measured: 373 ms blocked, timer fired 0×.

So: direct mode is a good fit for a CLI, a script, a batch job, or a Bun server
whose llmux traffic is streaming. If your server makes blocking unary calls on
its request path, **use the sidecar** — a 900 ms completion is otherwise a
900 ms freeze of everything else that process was doing.

Choose the sidecar too if you need per-tenant virtual keys, budgets or model
allow-lists: those are enforced by the HTTP shell's auth middleware, and there
is deliberately no authentication on the C ABI because an in-process host is
already inside the trust boundary.

Tested on **Bun 1.3.14, darwin/arm64**.

---

## Direct

```ts
import { abiVersion, Gateway } from "./index.ts";

await using gw = Gateway.open({ expectVersion: abiVersion() });

const answer = gw.call("chat", { model: "gpt-4o-mini", messages: [{ role: "user", content: "hi" }] });

for await (const chunk of gw.stream({ model: "gpt-4o-mini", messages: [{ role: "user", content: "hi" }] })) {
  process.stdout.write(chunk.choices?.[0]?.delta?.content ?? "");
}
```

`Gateway.open` is inert: no goroutines, no sockets (unless your config names a
Postgres DSN), no price sync, no spend flusher. Nothing happens until you call.

`Gateway` implements `Symbol.asyncDispose` — not `Symbol.dispose` — because
tearing a stream worker down is asynchronous. Use `await using`. `close()` is
idempotent, as `llmux_close` is.

No permission flags are involved. That is a genuine difference from Deno, where
FFI is gated behind `--allow-ffi`: **`bun:ffi` has no permission model**, so any
dependency in your process can dlopen anything.

- `gw.call(method, request)` — blocking; see "Which one should I use?" above.
- `gw.stream(request, options?)` — an async generator. `break`, `return`, an
  exception in the loop body, or `options.signal` firing all stop the stream —
  see "Cancellation" below.
- `gw.cancel()` — the low-level escape hatch `stream()`'s `signal` is built on.
  Aborts every call in flight on this handle and leaves it open. See
  "Cancellation".

### Cancellation

llmux v0.1.5 added a seventh ABI symbol, `llmux_cancel`
(`ffi/include/llmux.h`). Before it, the only way out of a call blocked in
`llmux_call` or `llmux_stream` was `llmux_close`, which destroys the gateway
and every other stream running on it. `llmux_cancel` aborts what is running
and leaves the handle open — `call("models")` right after a cancel succeeds
normally.

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

**How this reaches across the Worker boundary**: `stream()` runs
`llmux_stream` on a Worker (see "One gateway, two threads" below), so a plain
in-process flag cannot interrupt it from the main thread the way it could if
everything ran on one thread. `llmux_cancel` does not need to — it is called
from the MAIN thread's own `dlopen`'d symbol table, targeting the SAME handle
number the Worker is blocked on. That works, and is not a race, for the exact
reason the Worker is handed this gateway's handle rather than a second one: a
Bun `Worker` is a thread in the same process, `dlopen` of the same path
resolves to the library already loaded there, and `llmux_cancel` is documented
as safe "from another thread while the call is blocked". No postMessage has to
reach the Worker before the native call is interrupted — the shared `Atomics`
stop flag exists for the Worker's own bookkeeping (telling a self-inflicted
cancel from an external one — see `stream-worker.ts`), not for reaching the
library.

**`llmux_cancel` is per-HANDLE, not per-stream or per-call.** It aborts EVERY
call in flight on the gateway it is given — every `stream()`, every `call()`
currently running. `stream()`'s `signal` option does not change that: aborting
the signal you gave to `streamA` also aborts `streamB` if both are running on
the same `Gateway`. **If you need independent cancellation for concurrent
streams, give each one its own gateway** — `Gateway.open()` is inert and cheap
enough to call per cancellation scope; do not share one handle across scopes
and expect only one of them to stop.

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

An already-aborted `AbortSignal` rejects before any work starts at all — no
Worker is even spawned. See "What cancelling actually stops" below for what is
and is not measured on this machine.

### Memory and handles

- Every `char*` llmux returns — results **and** error messages — goes through
  `llmux_free` before the value reaches you, on the error path included.
- `llmux_call` is declared `FFIType.ptr`, not `FFIType.cstring`, precisely so
  bun cannot decode the result into a string and drop the pointer we still have
  to free. `llmux_abi_version` *is* declared `cstring`, which is correct there
  and only there: it returns static storage that must not be freed.
- The worker's `JSCallback` is closed only after `llmux_stream` has unwound.
- The generator's `finally` runs on `break` and on `throw` as well as on normal
  completion: it raises the shared stop flag, calls `llmux_cancel` on the same
  handle from the main thread (see "Cancellation" above), waits for the
  native call to finish, and only then terminates the worker.

### One gateway, two threads

The stream worker is handed **this gateway's handle**, not a second gateway.
That is sound, not a trick: a Bun `Worker` is a thread in the same process, so
`dlopen` in the worker resolves the library already loaded there, and
`llmux.h` states that a handle is safe to use from several threads at once. One
gateway means one cache and one set of spend counters — which is not what you
would get if the worker constructed its own.

The chunk callback runs on the worker's own thread, synchronously, inside
`llmux_stream`. So `JSCallback` is created **without** `threadsafe: true`: there
is no cross-thread call to marshal. `ffi/ctest/smoke.c` asserts that thread
identity with `pthread_self()` on every CI run, which is why this binding can
rely on it instead of coding defensively around a problem that does not exist.

### Run it

```
../../scripts/build-ffi.sh
bun run examples/direct.ts
```

```
bun        1.3.14 on darwin/arm64
library    /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi        0.1.2

handle     1

models       anthropic/claude-3-5-sonnet, anthropic/claude-3-5-haiku, deepseek/deepseek-chat, openai/gpt-4o, openai/gpt-4o-mini, google/gemini-1.5-pro, google/gemini-1.5-flash
chat         "the quick brown fox jumps over the lazy dog"
             blocked the event loop for 373 ms; timer fired 0x

stream      the quick brown fox jumps over the lazy dog
             10 chunks, event loop ticked 171x during the stream

break       consumed 3 chunks; the C callback fired 4x (10 = the whole answer)
            no error raised — stopping is your decision, not a failure

error       no route for model "no-such-model" (providers: fake)
stream:true llmux: "stream": true is not valid for llmux_call; use llmux_stream
closed      llmux gateway is closed
```

**This capture predates `llmux_cancel` and is not proof of anything about
this change.** It is left here because it is still true of the parts it
covers (models/chat/stream), but the `abi 0.1.2` line, the `break` row, and
the absence of a `cancel` row are all now stale: the library on this machine
reports `0.1.5`, `stream()` and `gw.cancel()` were rewired to call
`llmux_cancel`, and `examples/direct.ts` gained an AbortSignal-based
cancellation demo (see "Cancellation" above). **None of that has been run on
Bun.** There is no Bun runtime on the machine this change was written on —
`bun` is absent from `PATH` — so the new code is type-checked
(`bun run check`, i.e. `tsc --noEmit`, passes) but not executed, and no
`break` count, no `cancel` count, and no `GET /generated` count for the new
behaviour is reported anywhere in this file. Run
`../../scripts/build-ffi.sh && bun run examples/direct.ts` on a machine with
Bun installed to get real numbers, then replace this whole block (including
this notice) with fresh output the way [`../deno/README.md`](../deno/README.md)'s
equivalent section was.

No API key and no network beyond loopback: the example spawns
[`examples/fake-upstream.mjs`](examples/fake-upstream.mjs), an
OpenAI-compatible fixture that prints the llmux config routing `demo` at
itself, and the cancellation demo also `fetch()`es that same fixture's own
`GET /generated` to see what the provider actually produced.

---

## Sidecar

```ts
import { Sidecar } from "./index.ts";

await using side = await Sidecar.start();
// side.baseURL        -> http://127.0.0.1:<port>
// side.openaiBaseURL  -> http://127.0.0.1:<port>/v1
```

The same contract every llmux SDK implements: resolve the binary
(`options.binary` → `LLMUX_BINARY` → `llmux` on `PATH`), pick a free
`127.0.0.1` port, launch with `LLMUX_ADDR=127.0.0.1:<port>` inheriting the
environment so provider keys pass through, poll `/health` until it answers 200.

Two behavioural differences from direct mode:

- The child inherits stdio, so llmux's logs land on your stderr, interleaved
  with your own output.
- `llmux serve` **syncs a price catalog over the network at startup**. The
  library does not — a shared library loaded into someone else's process must
  not start background traffic they did not ask for.

### Run it

```
GOFLAGS=-mod=mod go build -o /tmp/llmux ../../cmd/llmux
LLMUX_BINARY=/tmp/llmux bun run examples/sidecar.ts
```

```
bun        1.3.14 on darwin/arm64
sidecar    http://127.0.0.1:59208
openai     http://127.0.0.1:59208/v1

chat         "the quick brown fox jumps over the lazy dog"

stream      the quick brown fox jumps over the lazy dog
             10 chunks

error       HTTP 404 {"error":{"message":"no route for model \"no-such-model\" (providers: fake)","type":"invalid_request_error","c
```

(llmux's own log lines are interleaved with that on a real run and are cut here.)

---

## What cancelling actually stops

A wrapper that turns a native callback into an async iterator has a failure mode
that looks like success: the consumer stops early, the loop exits, everything
looks cancelled — and the library ran to completion anyway, generating and
billing tokens nobody read. It was found in two other bindings in this suite, so
it is measured here rather than assumed.

**The backpressure measurements below are real but predate `llmux_cancel` —
they were taken against the shared-`Atomics`-flag-only implementation, before
this SDK gained the seventh ABI symbol.** `gw.stream(...)` exposes
`nativeChunks`, the number of times the C callback actually fired. The example
broke after 3 chunks of a 10-chunk answer and printed both numbers. Measured
on Bun 1.3.14, darwin/arm64, before this change:

| upstream pace | consumed | C callback fired |
|---|---|---|
| 40 ms/chunk, no backpressure | 3 | 4 |
| as fast as the socket allows, **no backpressure** | 3 | **10 — the entire answer** |
| 40 ms/chunk, with backpressure | 3 | **4** |
| as fast as the socket allows, with backpressure | 3 | **5** |

The middle row was the bug, reproduced. `postMessage` from the worker is
fire-and-forget, so with a fast upstream the worker ran the whole completion
before the main thread got round to breaking — a `take(3)` that silently paid
for ten chunks. The fix that produced this table is in
[`stream-worker.ts`](stream-worker.ts): a second Int32 in the shared control
block counts chunks the consumer has actually pulled, and the callback blocks
in `Atomics.wait` until it is no more than one chunk ahead. Blocking there is
legal precisely because it is the worker thread — `Atomics.wait` throws on a
main thread — and that thread is already parked inside `llmux_stream`. The
wait is bounded at 50 ms per iteration and re-checks the stop flag, so a lost
notify degrades to a poll instead of a hang.

That backpressure mechanism bounded the overrun; it could not eliminate it,
because a shared flag only stops the NEXT callback invocation — it cannot
interrupt a network read already in progress. `llmux_cancel` closes that
remaining gap: `break`ing, `return`ing, throwing out of, or aborting the
signal on `gw.stream()`'s iterator now also calls `llmux_cancel` from the
main thread directly onto the handle the worker is blocked in (see
"Cancellation" above) — the same fix Deno's binding measured going from a
4-callback overrun down to 3 (or all the way to `generated: 3 of 10` on the
upstream's own count, using an `AbortSignal` fired from outside the loop) at
a realistic per-chunk delay. See
[`../deno/README.md`](../deno/README.md#what-cancelling-actually-stops) for
that measurement in full, including the one honest exception (a synthetic
zero-delay flood, where the fix cannot outrun data already sitting in a
socket buffer).

**None of the above has been re-measured on Bun for this change.** There is
no Bun runtime on the machine this was written on, so the backpressure table
is left as the last real Bun measurement taken (against the pre-`llmux_cancel`
code), not as a claim about the code in this version. Re-running
`examples/direct.ts`'s `break` and `cancel` sections on a machine with Bun
installed, and reporting both `nativeChunks` and the upstream's own
`GET /generated` count the way the cancel section of the example already
prints them, is the next thing that needs to happen here — not a new number
guessed from the Deno result, which runs on a different FFI binding
(`nonblocking: true` symbols and no Worker at all) and is not guaranteed to
land on the identical count.

Tokens already served are metered either way — cancelling stops the *next*
chunk, not the ones already delivered. See "Cancellation" above for which
outcome raises an error and which does not.

---

## The costs of direct mode

Not footnotes. These are properties of `-buildmode=c-shared`;
[`ffi/README.md`](../../ffi/README.md) is the long version.

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   signal handlers. Measured: Go replaces five — `SIGSEGV`, `SIGBUS`, `SIGFPE`,
   `SIGPIPE`, `SIGURG` — and leaves three more in place with `SA_ONSTACK` added
   (`SIGILL`, `SIGXFSZ`, `SIGUSR2`). Go chains to a pre-existing handler in most
   cases; "most" is the honest word. Bun's JavaScriptCore installs its own signal
   handling, and a crash reporter attached to the process is a third party in
   that negotiation.

   **`SIGPROF` is not touched**, so sampling profilers that use it are
   unaffected. The measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).

2. **It is not fork-safe.** After `fork()` without `exec()` the Go runtime in
   the child is broken. `Bun.spawn` always execs, so ordinary Bun code cannot
   hit this. It becomes real under a pre-fork supervisor that loads your app in
   the parent, or a native module that forks. The rule is unchanged: **load the
   library after the fork, in the worker, never in the master.**

3. **The library is 12–17 MB.** Measured: 12,823,104 bytes on darwin/arm64,
   17,356,264 bytes on linux/arm64.

4. **Prebuilt libraries exist for darwin/arm64 and linux/arm64 only.**
   linux/amd64 is built and tested in CI but not shipped from a developer
   machine. **windows/amd64 and darwin/amd64 do not exist — no `llmux.dll` has
   ever been produced by anyone.** Bun runs on Windows; direct mode does not.
   Use the sidecar there. (openrate's shared library has a *different* platform
   matrix — see `openrate/sdks/bun/README.md`. One does not cover the other.)

5. **Latency is not the reason to embed.** Measured in `ffi/bench`: the boundary
   is ~4 µs in-process against ~46 µs over loopback, and a whole chat call is
   ~80–92 µs against ~102–109 µs. Against a model answering in hundreds of
   milliseconds that is noise. And streaming here adds a Worker hop per chunk,
   which eats some of it back. The reasons to embed are no second process, no
   port and no loopback surface — not speed.

6. **Two Go libraries means two Go runtimes.** If this process also loads
   libopenrate you get two independent runtimes with two GCs. It works; it is
   not free.

7. **A Worker per stream.** `gw.stream()` spawns one and terminates it when the
   iterator finishes. That is cheap next to an LLM completion but it is not
   free, and a process running thousands of concurrent streams should be talking
   to the sidecar instead.

---

## Layout and checks

```
sdks/bun/
  index.ts                    Gateway (direct) + Sidecar
  stream-worker.ts            the worker half of llmux_stream
  examples/direct.ts          direct mode, end to end
  examples/sidecar.ts         sidecar mode, end to end
  examples/fake-upstream.mjs  OpenAI-compatible fixture, so both run offline
```

Runtime dependencies: **none**. `bun:ffi`, `node:worker_threads` and `Bun.spawn`
are all in the runtime; `@types/bun` is a devDependency for the type check only.

```
bun install
bun run check        # tsc --noEmit, reusing the TypeScript pinned by sdks/node
bun run example:direct
bun run example:sidecar
```

`check` deliberately does not add a second `typescript` dependency to this repo:
`sdks/node` pins it exactly at 6.0.3 and `sdks/node/scripts/check-lint-config.mjs` asserts
that pin across every `package.json` in the tree. Borrowing that binary keeps one
pinned compiler in the repo instead of two that can drift.

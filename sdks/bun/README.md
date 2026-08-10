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

### Memory and handles

- Every `char*` llmux returns — results **and** error messages — goes through
  `llmux_free` before the value reaches you, on the error path included.
- `llmux_call` is declared `FFIType.ptr`, not `FFIType.cstring`, precisely so
  bun cannot decode the result into a string and drop the pointer we still have
  to free. `llmux_abi_version` *is* declared `cstring`, which is correct there
  and only there: it returns static storage that must not be freed.
- The worker's `JSCallback` is closed only after `llmux_stream` has unwound.
- The generator's `finally` runs on `break` and on `throw` as well as on normal
  completion: it raises the shared stop flag, waits for the native call to
  finish, and only then terminates the worker.

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

Those two tick counts, `0x` and `171x`, are the argument for reading the
"which one" section above rather than skipping it.

No API key and no network: the example spawns
[`examples/fake-upstream.mjs`](examples/fake-upstream.mjs), an
OpenAI-compatible fixture that prints the llmux config routing `demo` at itself.

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

## What `break` actually stops

A wrapper that turns a native callback into an async iterator has a failure mode
that looks like success: the consumer stops early, the loop exits, everything
looks cancelled — and the library ran to completion anyway, generating and
billing tokens nobody read. It was found in two other bindings in this suite, so
it is measured here rather than assumed.

`gw.stream(...)` exposes `nativeChunks`, the number of times the C callback
actually fired. The example breaks after 3 chunks of a 10-chunk answer and
prints both numbers. Measured on Bun 1.3.14, darwin/arm64:

| upstream pace | consumed | C callback fired |
|---|---|---|
| 40 ms/chunk, no backpressure | 3 | 4 |
| as fast as the socket allows, **no backpressure** | 3 | **10 — the entire answer** |
| 40 ms/chunk, with backpressure | 3 | **4** |
| as fast as the socket allows, with backpressure | 3 | **5** |

The middle row is the bug, reproduced. `postMessage` from the worker is
fire-and-forget, so with a fast upstream the worker ran the whole completion
before the main thread got round to breaking — a `take(3)` that silently paid
for ten chunks.

The fix is in [`stream-worker.ts`](stream-worker.ts): a second Int32 in the
shared control block counts chunks the consumer has actually pulled, and the
callback blocks in `Atomics.wait` until it is no more than one chunk ahead.
Blocking there is legal precisely because it is the worker thread — `Atomics.wait`
throws on a main thread — and that thread is already parked inside
`llmux_stream`. The wait is bounded at 50 ms per iteration and re-checks the stop
flag, so a lost notify degrades to a poll instead of a hang.

It costs something, and the cost should be stated: against an upstream with no
delay at all, backpressure took the full 10-chunk stream from 294 event-loop
ticks to 16, because the library now waits for the loop between chunks. Against
a realistic 40 ms/chunk upstream it made no measurable difference (318 ticks).
A real model is far slower than either fixture.

The residue is one chunk in flight — `llmux_stream` can only notice the stop
flag at the *next* chunk boundary, which the header says plainly. Tokens already
served are metered either way.

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

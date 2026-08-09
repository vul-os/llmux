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
| direct, the example | `--allow-ffi --allow-run --allow-env --allow-read` — run/read/env are for the fake upstream it spawns, not for llmux |
| sidecar | `--allow-run --allow-net --allow-env` (`--allow-read --allow-write` in the example, for its temporary config file) |

**`--unstable-ffi`**: not needed here. On Deno 2 the FFI API is stable and
`--allow-ffi` alone works — verified by running `examples/direct.ts` with
exactly `--allow-ffi --allow-run --allow-env --allow-read` and nothing else. On
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
- `gw.stream(request)` — an async generator. `break` stops the stream.

Everything the library allocates is released: `takeString` frees each result
with `llmux_free` before returning the string, `takeError` does the same for the
error message, and the `UnsafeCallback` is closed only **after** the
`llmux_stream` promise has settled — closing it while the library still holds
the pointer is how this becomes a segfault instead of an exception. The
generator's `finally` runs on `break` and on `throw` as well as on completion,
so the stop-and-wait sequence is not something the caller has to remember.

### Run it

```
../../scripts/build-ffi.sh
deno task example:direct
```

```
deno       2.7.11 on darwin/aarch64
library    /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi        0.1.2

handle     1

models       deepseek/deepseek-chat, openai/gpt-4o-mini, anthropic/claude-3-5-haiku, openai/gpt-4o, anthropic/claude-3-5-sonnet, google/gemini-1.5-pro, google/gemini-1.5-flash
chat         "the quick brown fox jumps over the lazy dog"
             took 393 ms; the event loop ticked 171x meanwhile

stream      the quick brown fox jumps over the lazy dog
             10 chunks, event loop ticked 168x during the stream

break       stopped after 3 chunks, no error raised

error       no route for model "no-such-model" (providers: fake)
stream:true llmux: "stream": true is not valid for llmux_call; use llmux_stream
closed      llmux gateway is closed
```

Those tick counts are the whole argument: a 1 ms timer fires 171× across a
393 ms call. The same measurement on Node is `0`.

No API key and no network — the example spawns
[`examples/fake-upstream.mjs`](examples/fake-upstream.mjs), an
OpenAI-compatible fixture that prints the llmux config routing `demo` at itself.

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

## The costs of direct mode

Not footnotes. These are properties of `-buildmode=c-shared`;
[`ffi/README.md`](../../ffi/README.md) is the long version.

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   handlers for `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPROF` and others. Go chains
   to a pre-existing handler in most cases; "most" is the honest word. Deno's V8
   also installs signal handling, and a native profiler attached to the process
   is a third party in that negotiation.

2. **It is not fork-safe.** After `fork()` without `exec()` the Go runtime in
   the child is broken. Deno has no `fork()` in its API — `Deno.Command` always
   execs — so ordinary Deno code cannot hit this. It becomes real if you embed
   Deno (`deno_core`) inside a host that pre-forks, or run under a supervisor
   that forks after loading. The rule is unchanged: **load the library after the
   fork, in the worker, never in the master.**

3. **The library is 12–17 MB.** Measured: 12,769,346 bytes on darwin/arm64,
   17,348,392 bytes on linux/arm64.

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

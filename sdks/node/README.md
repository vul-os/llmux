# llmux for Node

Two ways to use llmux from Node, both supported, both with a runnable example
in [`examples/`](examples):

| | module | what it is | streaming | event loop |
|---|---|---|---|---|
| **Sidecar** | `require("llmux")` | `llmux serve` in a child process, HTTP over loopback | SSE, `for await` | untouched |
| **Direct** | `require("llmux/direct")` | libllmux loaded into this process over the C ABI | callback per chunk | **blocked for the whole call** |

## Which one should I use?

**The sidecar, unless you know why not.** On Node specifically — not as a
general truth about embedding — direct mode cannot be made asynchronous. Every
direct call blocks the event loop from the moment it starts until the provider
has answered, streaming included. The measurement and the reason are below
under [Why direct mode on Node is synchronous](#why-direct-mode-on-node-is-synchronous).

Direct mode is the right answer for CLIs, scripts, batch jobs, tests and build
tooling: one process, no port, no loopback listener, nothing to supervise.

It is the wrong answer for an HTTP server. A 900 ms completion is a 900 ms
freeze of every other request that process was serving.

This mirrors `patala-go`, which names cackle as the repo that correctly chose
the sidecar. Choosing the sidecar is an outcome of reading this page, not a
failure to deliver an FFI binding.

---

## Sidecar

```js
const llmux = require("llmux");

const base = await llmux.start();          // free port, spawn, poll /health
const client = await llmux.OpenAI();       // an `openai` client pointed at it
```

`start()` is lazy, idempotent and singleton. It resolves the binary
(`LLMUX_BINARY` → bundled `bin/llmux` → `llmux` on `PATH`), picks a free
`127.0.0.1` port, launches with `LLMUX_ADDR=127.0.0.1:<port>` inheriting the
environment so provider keys pass through, and polls `/health`. `stop()` kills
it, and it is wired to `process.on("exit")` and `SIGINT` as well.

Streaming is an ordinary SSE body, so `for await (const bytes of res.body)`
works with no glue — see [`examples/sidecar.ts`](examples/sidecar.ts).

Two behavioural differences from direct mode, worth knowing:

- The child inherits stdio, so llmux's own logs land on your stderr.
- `llmux serve` syncs a **price catalog over the network at startup**. The
  library does not: a shared library loaded into someone else's process must not
  start background traffic they did not ask for.

### Run it

```
npm install
npm run build
GOFLAGS=-mod=mod go build -o /tmp/llmux ../../cmd/llmux
LLMUX_BINARY=/tmp/llmux node examples/sidecar.ts
```

```
node       v24.12.0 on darwin/arm64
sidecar    http://127.0.0.1:52927
openai     http://127.0.0.1:52927/v1

chat         "the quick brown fox jumps over the lazy dog"

stream      the quick brown fox jumps over the lazy dog
             10 chunks

error       HTTP 404 {"error":{"message":"no route for model \"no-such-model\" (providers: fake)","type":"invalid_request_error","code":"mode
```

(llmux's own log lines are interleaved with that on a real run; they are cut
here for readability.)

---

## Direct

```js
const { Gateway, abiVersion } = require("llmux/direct");

using gw = Gateway.open({ expectVersion: abiVersion() });

const answer = gw.call("chat", { model: "gpt-4o-mini", messages: [{ role: "user", content: "hi" }] });

gw.stream({ model: "gpt-4o-mini", messages: [{ role: "user", content: "hi" }] }, (chunk) => {
  process.stdout.write(chunk.choices?.[0]?.delta?.content ?? "");
  // return false to stop early — that is not an error
});
```

`Gateway.open` is inert: no goroutines, no sockets (unless your config names a
Postgres DSN), no price sync, no spend flusher. Nothing happens until you call.

`Gateway` implements `Symbol.dispose`, so `using` closes the handle on every
exit path out of the block including a throw. Node 24 supports `using` natively;
if you are on an older Node, call `gw.close()` in a `finally`. `close()` is
idempotent, as `llmux_close` is.

Memory and handles are the caller's problem in C and this module makes them not
yours:

- every `char*` llmux returns — results **and** error messages — goes through
  `llmux_free` before the value reaches you, including on the error path;
- `llmux_call` is declared as returning `void*`, not `char*`, precisely so koffi
  cannot decode it into a string and discard the pointer we still have to free;
- the streaming callback is unregistered only **after** `llmux_stream` has
  unwound, never while the library still holds the pointer;
- an exception thrown by your chunk callback is caught, the stream is asked to
  stop, and the exception is rethrown after the native call returns — it never
  unwinds through a Go call frame.

### Run it

```
npm install
npm run build
../../scripts/build-ffi.sh
node examples/direct.ts
```

```
node       v24.12.0 on darwin/arm64
library    /Users/pc/code/vulos/llmux/dist/ffi/darwin_arm64/libllmux.dylib
abi        0.1.2

handle     1

models       anthropic/claude-3-5-haiku, google/gemini-1.5-flash, deepseek/deepseek-chat, openai/gpt-4o, openai/gpt-4o-mini, anthropic/claude-3-5-sonnet, google/gemini-1.5-pro
chat         "the quick brown fox jumps over the lazy dog"
             blocked the event loop for 375 ms; timer fired 0x

stream      the quick brown fox jumps over the lazy dog
             10 chunks

break       stopped after 3 chunks, no error raised

error       no route for model "no-such-model" (providers: fake)
stream:true llmux: "stream": true is not valid for llmux_call; use llmux_stream
closed      llmux gateway is closed
```

The example needs no API key and no network: it spawns
[`examples/fake-upstream.mjs`](examples/fake-upstream.mjs), a 70-line
OpenAI-compatible fixture that prints the llmux config routing `demo` at itself.
`FAKE_DELAY_MS=40` makes it answer at a realistic pace, which is what the "375
ms, 0 ticks" line above is measuring.

---

## Why direct mode on Node is synchronous

`llmux_stream` blocks its caller until the stream ends and runs the chunk
callback on that same thread ([`ffi/include/llmux.h`](../../ffi/include/llmux.h)
documents this and `ffi/ctest/smoke.c` asserts it with `pthread_self()`). So an
async iterator needs the call to happen on some thread that is not Node's main
thread. Both candidate threads were tried on darwin/arm64, Node v24.12.0,
koffi 3.1.4, and **both leave the process unable to exit**:

| approach | streaming worked? | process exited? |
|---|---|---|
| `worker_threads` worker calling libllmux | yes — 6 chunks, main thread ticked 82× | **no** — the worker's `exit` event never fires |
| koffi `.async` (libuv threadpool) | yes — 5 chunks, main thread ticked 3× | **no** — hangs after the last statement |
| main thread, synchronous | yes | yes |

Minimal reproduction, no llmux logic involved at all — a worker that loads a Go
c-shared library and calls one function that returns a constant string:

```js
import { Worker, isMainThread, parentPort } from "node:worker_threads";
if (!isMainThread) {
  const koffi = (await import("koffi")).default;
  const lib = koffi.load("…/libopenrate-darwin-arm64.dylib");
  parentPort.postMessage(lib.func("const char *openrate_abi_version()")());
} else {
  const w = new Worker(new URL(import.meta.url));
  console.log("worker said:", await new Promise((r) => w.on("message", r)));
  console.log("worker exited with", await new Promise((r) => w.on("exit", r))); // never prints
}
```

It prints `worker said: 0.1.2` and then hangs forever. The **control** —
identical code against `/usr/lib/libSystem.B.dylib` and `atoi` — prints
`worker exited with 0` and returns to the shell. It reproduces against two
independent Go c-shared libraries (libllmux and libopenrate) and not against a
C library, so this is a Go-runtime × non-main-thread interaction, not a koffi
bug and not an llmux bug: a thread that has entered the Go runtime cannot be
joined, and Node joins its worker threads on the way out.

The same experiment on the other two JS runtimes in this suite comes out
differently, which is why their SDKs make different choices:

| runtime | off-main-thread FFI | streaming |
|---|---|---|
| Deno | `nonblocking: true` symbol + `Deno.UnsafeCallback` | async iterator, event loop free, clean exit |
| Bun | `node:worker_threads` worker | async iterator, event loop free, clean exit |
| **Node** | none that terminates | **callback only, blocking — use the sidecar** |

So `Gateway.stream` takes a callback rather than returning an async iterator.
The alternative — buffering the whole completion and replaying it as chunks —
would make `for await` look right while destroying time-to-first-token, which
[`ffi/README.md`](../../ffi/README.md) calls out by name as worse than an honest
HTTP call.

---

## Why koffi

Node has no FFI in its standard library, so direct mode needs one of three
things. The choice is [`koffi`](https://koffi.dev/), declared as an **optional
peer dependency** so the sidecar path installs with no native code at all.

- **`node-ffi-napi`** — effectively unmaintained; its last release predates
  several Node majors and it does not build cleanly on current Node. Not viable.
- **A hand-written N-API addon** — the honest alternative, and it would give us
  `napi_threadsafe_function`. It was rejected on cost: it means node-gyp and a C
  toolchain at install time, or `prebuildify` artifacts for every
  platform × Node-ABI pair, for a six-function ABI. It would also not solve the
  problem above — the thread that cannot be joined is a property of the Go
  runtime, not of how we cross the boundary — so it would buy a build pipeline
  and no async streaming.
- **`koffi`** — MIT, actively released (3.1.4 at the time of writing), ships
  prebuilt binaries so `npm install` needs no compiler, ~1.9 MB installed, and
  its declarative C prototypes let this binding be a transcription of
  `llmux.h` rather than a reimplementation of it.

The cost of the dependency is real and worth naming: koffi is native code
running in your process, and a bug in it is a segfault, not an exception.

---

## The costs of direct mode

Not footnotes. All of these are properties of `-buildmode=c-shared`, and
[`ffi/README.md`](../../ffi/README.md) is the long version.

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   signal handlers for `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPROF` and others. Go
   chains to a pre-existing handler in most cases; "most" is the honest word.
   Node installs its own crash handlers, and a native profiler or crash reporter
   attached to your process is a third party in that negotiation.

2. **It is not fork-safe.** After `fork()` without `exec()` the Go runtime in the
   child is broken. Node's own `child_process` and `cluster` always `exec`, so
   ordinary Node code is not at risk. The concrete victims are native modules
   and deployment wrappers that fork the interpreter: `posix.fork()` from a
   native addon, and any pre-fork supervisor that loads your app in the parent
   and forks workers. The rule is the same as everywhere else — **load the
   library after the fork, in the worker, never in the master.**

3. **The library is 12–17 MB.** Measured: 12,769,346 bytes on darwin/arm64,
   17,348,392 bytes on linux/arm64.

4. **Prebuilt libraries exist for darwin/arm64 and linux/arm64 only.**
   linux/amd64 is built and tested in CI but not shipped from a developer
   machine. **windows/amd64 and darwin/amd64 do not exist — no `llmux.dll` has
   ever been produced by anyone.** If your Node app runs on Windows, direct mode
   is not available to you at all; the sidecar is. (openrate's shared library has
   a *different* platform matrix — see `openrate/sdks/node/README.md`; do not
   assume one covers the other.)

5. **Latency is not the reason to embed.** Measured in `ffi/bench`: the boundary
   itself is ~4 µs in-process against ~46 µs over loopback, and a whole chat call
   is ~80–92 µs against ~102–109 µs. Against a model that answers in hundreds of
   milliseconds that is noise. The reasons to embed are no second process, no
   port, and no loopback surface — not speed.

6. **Two Go libraries means two Go runtimes.** If this process also loads
   libopenrate, you get two independent runtimes with two GCs. It works; it is
   not free.

---

## Layout

```
sdks/node/
  index.ts                    the sidecar (spawn + health poll + OpenAI helper)
  direct.ts                   the C ABI binding
  examples/direct.ts          direct mode, end to end
  examples/sidecar.ts         sidecar mode, end to end
  examples/fake-upstream.mjs  OpenAI-compatible fixture, so both run offline
  test/sidecar.test.ts        the sidecar contract suite
```

`examples/fake-upstream.mjs` is deliberately plain JavaScript: the Deno and Bun
SDKs spawn the same fixture, and `node:http` is the one HTTP server API all
three runtimes implement. SDK source is TypeScript; this file is a fixture, and
it is linted (see `eslint.config.mjs`) rather than left as an unchecked island.

## Checks

```
npm run lint                 # eslint, type-aware (strictTypeChecked)
npm run typecheck            # tsc over index.ts + direct.ts + fixtures + tests
npm run typecheck:examples   # tsc over examples/ (ESM, its own tsconfig)
npm run check:lint-config    # proves the lint config resolves type information
npm test                     # the sidecar contract suite
```

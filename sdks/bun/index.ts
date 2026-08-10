/**
 * llmux for Bun — both modes in one module.
 *
 * DIRECT (in-process, the C ABI in `ffi/include/llmux.h`):
 *
 * ```ts
 * import { Gateway } from "./index.ts";
 *
 * await using gw = Gateway.open();
 * const answer = gw.call("chat", { model: "gpt-4o-mini", messages: [...] });
 * for await (const chunk of gw.stream({ model: "gpt-4o-mini", messages: [...] })) { ... }
 * ```
 *
 * SIDECAR (out-of-process, `llmux serve` over loopback):
 *
 * ```ts
 * await using side = await Sidecar.start();
 * const res = await fetch(side.openaiBaseURL + "/chat/completions", { ... });
 * ```
 *
 * Bun's FFI (`bun:ffi`) is built in — no third-party binding — but it has no
 * asynchronous call mode: every symbol runs on the thread that calls it. So
 * `call()` here is synchronous and blocks the event loop, while `stream()` runs
 * `llmux_stream` on a Worker and hands you a real async iterator. Both facts are
 * measured in README.md rather than asserted.
 *
 * JSON in, JSON out — the same JSON the HTTP API uses.
 *
 * @module
 */

import { Worker } from "node:worker_threads";
import { CString, dlopen, FFIType, ptr } from "bun:ffi";
import type { Pointer } from "bun:ffi";
import type { StreamWorkerData } from "./stream-worker.ts";

// ---------------------------------------------------------------------------
// Finding libllmux
// ---------------------------------------------------------------------------

function libFileName(): string {
  if (process.platform === "darwin") return "libllmux.dylib";
  if (process.platform === "win32") return "llmux.dll";
  return "libllmux.so";
}

function goArch(): string {
  return process.arch === "arm64" ? "arm64" : process.arch === "x64" ? "amd64" : process.arch;
}

function goOS(): string {
  return process.platform === "win32" ? "windows" : process.platform;
}

/**
 * Where the shared library will be loaded from: `LLMUX_LIB`, else a repo
 * checkout's `dist/ffi/<goos>_<goarch>/`, else the bare name for the system
 * loader.
 *
 * There is no prebuilt library for windows/amd64 or darwin/amd64. This function
 * will still name one; `dlopen` then fails with the loader's own message.
 */
export function resolveLibrary(explicit?: string): string {
  if (explicit) return explicit;
  if (process.env.LLMUX_LIB) return process.env.LLMUX_LIB;
  const candidate = new URL(
    `../../dist/ffi/${goOS()}_${goArch()}/${libFileName()}`,
    import.meta.url,
  ).pathname;
  return Bun.file(candidate).size > 0 ? candidate : libFileName();
}

// ---------------------------------------------------------------------------
// The ABI
// ---------------------------------------------------------------------------

const SYMBOLS = {
  llmux_abi_version: { args: [], returns: FFIType.cstring },
  llmux_new: { args: [FFIType.cstring, FFIType.ptr], returns: FFIType.u64 },
  llmux_close: { args: [FFIType.u64], returns: FFIType.void },
  llmux_cancel: { args: [FFIType.u64], returns: FFIType.void },
  llmux_call: {
    args: [FFIType.u64, FFIType.cstring, FFIType.cstring, FFIType.ptr],
    // FFIType.ptr, NOT cstring: bun would decode a cstring result into a JS
    // string and drop the pointer, leaving nothing to hand llmux_free.
    returns: FFIType.ptr,
  },
  llmux_free: { args: [FFIType.ptr], returns: FFIType.void },
} as const;

type Lib = ReturnType<typeof dlopen<typeof SYMBOLS>>;

const _libs = new Map<string, Lib>();

function load(libPath: string): Lib {
  const cached = _libs.get(libPath);
  if (cached) return cached;
  const lib = dlopen(libPath, SYMBOLS);
  _libs.set(libPath, lib);
  return lib;
}

const encoder = new TextEncoder();
const cstr = (s: string) => encoder.encode(s + "\0");

/**
 * A raw address read out of a `char**` slot, as bun's branded Pointer.
 * bun:ffi models pointers as an opaque number type; a value that came back
 * through a BigUint64Array has lost that brand and has to be given it back.
 */
function asPointer(address: bigint): Pointer | null {
  return address === 0n ? null : (Number(address) as unknown as Pointer);
}

/** Read a C string llmux allocated, then free it. Freeing is not optional. */
function takeString(lib: Lib, p: Pointer | null): string | null {
  if (!p) return null;
  try {
    return new CString(p).toString();
  } finally {
    lib.symbols.llmux_free(p);
  }
}

/** Turn a populated `char** err` into an Error, freeing the message. */
function takeError(lib: Lib, slot: BigUint64Array, fallback: string): Error {
  // Error strings are plain UTF-8 text, NOT JSON. Do not parse them.
  const msg = takeString(lib, asPointer(slot[0] ?? 0n));
  slot[0] = 0n;
  return new Error(msg ?? fallback);
}

/** The llmux version the loaded shared library was built from. */
export function abiVersion(libraryPath?: string): string {
  // Declared FFIType.cstring: bun decodes it and does not free it, which is
  // correct here and only here — llmux_abi_version returns static storage.
  return String(load(resolveLibrary(libraryPath)).symbols.llmux_abi_version());
}

// ---------------------------------------------------------------------------
// Gateway (direct)
// ---------------------------------------------------------------------------

export interface GatewayOptions {
  /**
   * An llmux configuration document — the same JSON `llmux serve` reads from
   * llmux.json. Omitted means built-in defaults plus the environment.
   */
  config?: string | Record<string, unknown> | null;
  /** Override the shared library path (otherwise {@link resolveLibrary}). */
  libraryPath?: string;
  /** Refuse to open unless the loaded library reports this version. */
  expectVersion?: string;
}

/** One `chat.completion.chunk`, as yielded by {@link Gateway.stream}. */
export interface ChatChunk {
  id?: string;
  object?: string;
  model?: string;
  choices?: {
    index?: number;
    delta?: { role?: string; content?: string };
    finish_reason?: string | null;
  }[];
  [k: string]: unknown;
}

export type Method = "chat" | "embed" | "models";

export interface StreamOptions {
  /**
   * Abort this stream early. Wired to `llmux_cancel`, which interrupts the
   * blocked native call directly rather than waiting for the worker's stop
   * flag to be noticed at the next chunk boundary — see {@link Gateway.cancel}.
   *
   * An already-aborted signal rejects before any work starts: no Worker is
   * even spawned.
   *
   * `llmux_cancel` is per-HANDLE, not per-stream: aborting this signal aborts
   * EVERY call in flight on this gateway, including some other stream you
   * started on the same `Gateway`. If independent cancellation matters, give
   * each cancellation scope its own `Gateway.open()`.
   */
  signal?: AbortSignal;
}

/**
 * What {@link Gateway.stream} returns: an async generator with one extra
 * property.
 *
 * `nativeChunks` counts how many times the C callback actually fired, which is
 * NOT the same as how many chunks you consumed. Compare the two after an early
 * `break` if you care whether the tokens you stopped reading were nonetheless
 * generated and metered. See README.md, "What cancelling actually stops".
 */
export interface ChunkStream extends AsyncGenerator<ChatChunk, void, unknown> {
  readonly nativeChunks: number;
}

/**
 * A gateway running in this process.
 *
 * Construction is INERT: no goroutines, no sockets (unless your configuration
 * names a Postgres DSN), no price sync, no spend flusher. Nothing happens until
 * you call.
 *
 * Async-disposable, because closing may have to terminate a stream worker:
 * `await using gw = Gateway.open()`.
 */
export class Gateway implements AsyncDisposable {
  #lib: Lib;
  #libPath: string;
  #h: bigint;
  #closed = false;

  private constructor(lib: Lib, libPath: string, h: bigint) {
    this.#lib = lib;
    this.#libPath = libPath;
    this.#h = h;
  }

  static open(opts: GatewayOptions = {}): Gateway {
    const libPath = resolveLibrary(opts.libraryPath);
    const lib = load(libPath);
    if (opts.expectVersion) {
      const got = abiVersion(libPath);
      if (got !== opts.expectVersion) {
        throw new Error(
          `libllmux at ${libPath} reports version ${got}, expected ${opts.expectVersion} — ` +
            "a stale library earlier on the load path is the usual cause",
        );
      }
    }
    const cfg = opts.config == null
      ? null
      : cstr(typeof opts.config === "string" ? opts.config : JSON.stringify(opts.config));
    const err = new BigUint64Array(1);
    const h = lib.symbols.llmux_new(cfg, ptr(err));
    if (h === 0n) throw takeError(lib, err, "llmux_new failed");
    return new Gateway(lib, libPath, h);
  }

  /** The registry key of this gateway. Diagnostic only; handles are never reused. */
  get handle(): bigint {
    return this.#h;
  }

  #live(): bigint {
    if (this.#closed) throw new Error("llmux gateway is closed");
    return this.#h;
  }

  /**
   * One unary call. Returns the parsed response document.
   *
   *   method     request                        result
   *   ------------------------------------------------------------------
   *   "chat"     OpenAI chat-completions body   a chat completion
   *   "embed"    OpenAI embeddings body         an embeddings response
   *   "models"   omit it                        an OpenAI model list
   *
   * A "chat" body with `"stream": true` is refused — use {@link stream}.
   *
   * BLOCKING. `bun:ffi` has no async call mode, so "chat" and "embed" freeze the
   * event loop for the whole upstream round trip. ("models" is answered from
   * memory in microseconds.) If that is unacceptable in your process, use the
   * sidecar — see README.md.
   */
  call(method: Method, request?: Record<string, unknown> | string | null): unknown {
    const lib = this.#lib;
    const err = new BigUint64Array(1);
    const body = request == null
      ? null
      : cstr(typeof request === "string" ? request : JSON.stringify(request));
    const res = lib.symbols.llmux_call(this.#live(), cstr(method), body, ptr(err));
    if (!res) throw takeError(lib, err, `llmux_call(${method}) failed`);
    return JSON.parse(takeString(lib, res) ?? "null");
  }

  /**
   * Stream a chat completion as an async iterator.
   *
   * ```ts
   * for await (const chunk of gw.stream({ model, messages })) { ... }
   *
   * const controller = new AbortController();
   * for await (const chunk of gw.stream({ model, messages }, { signal: controller.signal })) {
   *   if (shouldStop(chunk)) controller.abort();
   * }
   * ```
   *
   * Abandoning the iterator — `break`, a `return`, an exception in the loop
   * body, or `options.signal` firing — reaches `llmux_cancel`, called from
   * THIS (main) thread on the SAME handle the worker is blocked in
   * `llmux_stream` with. That works, and is not a race, for the same reason
   * the worker is handed this gateway's handle rather than a second one: a
   * Bun `Worker` is a thread in the same process, `dlopen` of the same path
   * resolves to the library already loaded there, and `llmux_cancel` is
   * documented as safe "from another thread while the call is blocked". No
   * message has to cross to the worker before the native call is interrupted
   * — only the `Atomics` stop flag does, and that is for the worker's own
   * bookkeeping (see stream-worker.ts), not for reaching the library.
   *
   * `break`ing out yourself is not an error: llmux does not hand your own
   * decision back to you as a failure, the same way a callback returning
   * non-zero is not one. An `options.signal` that fires while you are still
   * awaiting a chunk is different — that is somebody else's decision reaching
   * you asynchronously — so it rejects the pending `next()` with the signal's
   * abort reason (an `AbortError` `DOMException` if none was given), the error
   * every web-platform consumer already knows to catch. Tokens already served
   * are metered either way; see "What cancelling actually stops" in README.md.
   *
   * `llmux_stream` blocks its caller and runs the chunk callback on that same
   * thread, so this spawns a Worker per stream and runs the call there.
   */
  stream(request: Record<string, unknown> | string, options: StreamOptions = {}): ChunkStream {
    const counter = { n: 0 };
    const gen = this.#streamImpl(request, counter, options.signal) as ChunkStream;
    Object.defineProperty(gen, "nativeChunks", { get: () => counter.n, enumerable: true });
    return gen;
  }

  async *#streamImpl(
    request: Record<string, unknown> | string,
    counter: { n: number },
    signal: AbortSignal | undefined,
  ): AsyncGenerator<ChatChunk, void, unknown> {
    // An already-aborted signal fails before ANY work starts — no Worker
    // spawned, no dlopen inside it, nothing to tear down. This has to be the
    // very first thing that runs in the generator body, before `this.#live()`
    // even, so nothing is done on behalf of a request nobody wants.
    if (signal?.aborted) {
      throw signal.reason ?? new DOMException("stream aborted before it started", "AbortError");
    }

    const lib = this.#lib;
    const handle = this.#live();
    const body = typeof request === "string" ? request : JSON.stringify({ ...request, stream: true });
    // [0] stop, [1] acks — see stream-worker.ts.
    const control = new SharedArrayBuffer(8);
    const ctl = new Int32Array(control);
    const STOP = 0;
    const ACKS = 1;
    /** Tell the worker the consumer has pulled `n` chunks, releasing its wait. */
    const ack = (n: number) => {
      Atomics.store(ctl, ACKS, n);
      Atomics.notify(ctl, ACKS);
    };

    const data: StreamWorkerData = { libPath: this.#libPath, handle, request: body, control };
    const worker = new Worker(new URL("./stream-worker.ts", import.meta.url).href, { workerData: data });

    const queue: ChatChunk[] = [];
    let wake: (() => void) | null = null;
    let done = false;
    let failure: Error | null = null;
    // Set once WE ask llmux to stop this call — from the `finally` below (a
    // `break`/`return`/`throw` on the consumer's side) or from `onAbort`. It
    // is written into the SAME shared `ctl[STOP]` the worker already reads,
    // so stream-worker.ts can tell "this stream's own consumer asked for
    // this" from "something else cancelled the gateway out from under it" —
    // both come back from llmux_stream as rc=-1/"context canceled"
    // (ffi/include/llmux.h, on llmux_cancel) and only one of them should
    // reach this consumer as a thrown error.
    let cancelledByUs = false;
    // Set only by onAbort, never by the plain break/return/throw path: the
    // reason a still-awaiting consumer's `next()` should reject with.
    let abortReason: unknown = undefined;

    const ping = () => {
      const w = wake;
      wake = null;
      w?.();
    };

    // Both an abandoned iterator and a fired AbortSignal feed into this one
    // function, so `cancelledByUs` is set exactly once and `llmux_cancel` is
    // asked at most once per stream. Setting `ctl[STOP]` BEFORE calling
    // `llmux_cancel` matters: by the time the native call returns and the
    // worker inspects the flag to decide what to post back, it has to see
    // ours, not a stale read.
    const cancelNative = () => {
      if (cancelledByUs) return; // already asked; llmux_cancel is a no-op on repeat, but skip the call anyway
      cancelledByUs = true;
      Atomics.store(ctl, STOP, 1);
      Atomics.notify(ctl, ACKS); // release the worker if it is parked in the backpressure wait
      lib.symbols.llmux_cancel(handle);
    };
    const onAbort = () => {
      abortReason = signal!.reason ?? new DOMException("stream aborted", "AbortError");
      cancelNative();
      ping(); // wake a consumer parked on an empty queue instead of making it wait for the worker to unwind
    };
    signal?.addEventListener("abort", onAbort, { once: true });

    worker.on("message", (m: { chunk?: string; done?: boolean; error?: string | null; native?: number }) => {
      if (typeof m.native === "number") counter.n = m.native;
      if (typeof m.chunk === "string") {
        try {
          queue.push(JSON.parse(m.chunk) as ChatChunk);
        } catch {
          failure = new Error("llmux sent a chunk that is not JSON");
          Atomics.store(ctl, STOP, 1);
          Atomics.notify(ctl, ACKS);
        }
      } else if (m.done) {
        if (m.error) failure = new Error(m.error);
        done = true;
      }
      ping();
    });
    worker.on("error", (e: Error) => {
      failure = e;
      done = true;
      ping();
    });
    worker.on("exit", () => {
      done = true;
      ping();
    });

    let pulled = 0;
    try {
      for (;;) {
        const head = queue.shift();
        if (head !== undefined) {
          // Ack BEFORE yielding: the consumer's body may take a while, and the
          // point is to keep the library one chunk ahead, not zero.
          ack(++pulled);
          yield head;
          continue;
        }
        if (done) break;
        // An abort does not wait for the worker to unwind before telling the
        // consumer why: whatever is already queued drains first (already
        // delivered chunks stay delivered), then this breaks out immediately
        // rather than parking again.
        if (abortReason !== undefined) break;
        await new Promise<void>((resolve) => {
          wake = resolve;
        });
      }
      if (failure) throw failure;
      if (abortReason !== undefined) {
        throw abortReason instanceof Error ? abortReason : new DOMException(String(abortReason), "AbortError");
      }
    } finally {
      // Reached on `break` and `throw` too. Ask llmux to stop (unless it
      // already has finished on its own) and wait for the worker to unwind
      // before terminating it, so the native call finishes and closes its
      // JSCallback on its own thread rather than being killed mid-stream.
      signal?.removeEventListener("abort", onAbort);
      if (!done) cancelNative();
      if (!done) {
        await new Promise<void>((resolve) => {
          const check = () => (done ? resolve() : (wake = check));
          check();
        });
      }
      await worker.terminate();
    }
  }

  /**
   * Aborts every call in flight on this handle — every `stream()` and every
   * `call()` currently running, not just one of them, because `llmux_cancel`
   * is per-HANDLE. The handle itself stays open and usable: the next call
   * starts on a fresh context, and `call("models")` right after a cancel
   * succeeds normally.
   *
   * Safe to call from another thread — `stream()`'s Worker included — and
   * safe to call from inside a chunk callback, unlike {@link close}, which
   * must never be called from a callback because it waits (up to a few
   * seconds) on the very call running it. A no-op if nothing is running on
   * this handle, and a no-op if the handle is already closed: mirrors
   * `llmux_cancel`'s own contract of being harmless on an unknown handle, so
   * a caller racing this against `close()` from another thread never has to
   * guard the order.
   *
   * This is the low-level escape hatch. Reach for `stream()`'s `signal`
   * option first — it gives you a per-stream idiom without you having to
   * remember that this call reaches every stream on the gateway, not just
   * one.
   *
   * A `stream()` that asked to be cancelled itself (via `break`, `return`, an
   * exception, or its own `signal`) never throws for it — see `stream()`'s
   * doc. A `stream()` that did NOT ask for this and gets caught in the blast
   * radius anyway throws instead, on purpose: silently ending that consumer's
   * loop would look identical to it finishing normally, which is exactly the
   * kind of paper-over that would hide the one thing this method's docs are
   * trying to warn you about.
   */
  cancel(): void {
    this.#lib.symbols.llmux_cancel(this.#h);
  }

  /** Release the gateway, aborting any stream still running on it. Idempotent. */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#lib.symbols.llmux_close(this.#h);
    this.#h = 0n;
  }

  async [Symbol.asyncDispose](): Promise<void> {
    this.close();
    await Promise.resolve();
  }
}

// ---------------------------------------------------------------------------
// Sidecar
// ---------------------------------------------------------------------------

export interface SidecarOptions {
  /** Fixed port; default is an ephemeral free port. */
  port?: number;
  /** Path to an llmux config file (becomes LLMUX_CONFIG). */
  config?: string;
  /** Extra environment for the child. */
  env?: Record<string, string>;
  /** Health-check timeout in milliseconds (default 10000). */
  timeoutMs?: number;
  /** Binary to run; default LLMUX_BINARY, else "llmux" on PATH. */
  binary?: string;
}

function freePort(): number {
  const srv = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
  const { port } = srv;
  srv.stop(true);
  return port;
}

/**
 * `llmux serve` in a child process, started and supervised for you.
 *
 * Async-disposable, so `await using side = await Sidecar.start()` kills the
 * child on the way out of the block.
 */
export class Sidecar implements AsyncDisposable {
  readonly baseURL: string;
  readonly openaiBaseURL: string;
  #child: Bun.Subprocess;
  #stopped = false;

  private constructor(child: Bun.Subprocess, baseURL: string) {
    this.#child = child;
    this.baseURL = baseURL;
    this.openaiBaseURL = baseURL + "/v1";
  }

  static async start(opts: SidecarOptions = {}): Promise<Sidecar> {
    const port = opts.port ?? freePort();
    const addr = `127.0.0.1:${port}`;
    const binary = opts.binary ?? process.env.LLMUX_BINARY ?? "llmux";
    const env: Record<string, string | undefined> = {
      ...process.env,
      LLMUX_ADDR: addr,
      ...opts.env,
    };
    if (opts.config) env.LLMUX_CONFIG = opts.config;

    const child = Bun.spawn([binary], { env, stdout: "inherit", stderr: "inherit" });
    const base = `http://${addr}`;
    const deadline = Date.now() + (opts.timeoutMs ?? 10_000);
    for (;;) {
      try {
        const res = await fetch(base + "/health");
        await res.body?.cancel();
        if (res.status === 200) return new Sidecar(child, base);
      } catch {
        // not listening yet
      }
      if (Date.now() > deadline) {
        child.kill();
        throw new Error(`llmux did not become healthy on ${addr} in time`);
      }
      await Bun.sleep(50);
    }
  }

  /** Kill the child and wait for it. Idempotent. */
  async stop(): Promise<void> {
    if (this.#stopped) return;
    this.#stopped = true;
    this.#child.kill();
    await this.#child.exited;
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.stop();
  }
}

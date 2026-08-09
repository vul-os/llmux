/**
 * llmux for Deno — both modes in one module.
 *
 * DIRECT (in-process, the C ABI in `ffi/include/llmux.h`):
 *
 * ```ts
 * import { Gateway } from "./mod.ts";
 *
 * using gw = Gateway.open();
 * const answer = await gw.call("chat", { model: "gpt-4o-mini", messages: [...] });
 * for await (const chunk of gw.stream({ model: "gpt-4o-mini", messages: [...] })) {
 *   Deno.stdout.writeSync(new TextEncoder().encode(chunk.choices?.[0]?.delta?.content ?? ""));
 * }
 * ```
 *
 * Requires `--allow-ffi`. On Deno 1.x also `--unstable-ffi`; on Deno 2 the FFI
 * API is stable and `--allow-ffi` alone is enough (measured on 2.7.11).
 *
 * SIDECAR (out-of-process, `llmux serve` over loopback):
 *
 * ```ts
 * import { Sidecar } from "./mod.ts";
 *
 * await using side = await Sidecar.start();
 * const res = await fetch(side.openaiBaseURL + "/chat/completions", { ... });
 * ```
 *
 * Requires `--allow-run --allow-net --allow-env`.
 *
 * Deno is the best of the three JS runtimes for direct mode, for one concrete
 * reason: a symbol declared `nonblocking: true` runs on Deno's blocking-task
 * pool and Deno marshals the C callback back into the isolate, so streaming is a
 * real async iterator that never stalls the event loop AND the process still
 * exits cleanly. Node can do neither (see `../node/README.md`). No third-party
 * binding is involved: this is `Deno.dlopen` and `Deno.UnsafeCallback`.
 *
 * JSON in, JSON out — the same JSON the HTTP API uses.
 *
 * @module
 */

// ---------------------------------------------------------------------------
// Finding libllmux
// ---------------------------------------------------------------------------

function libFileName(): string {
  if (Deno.build.os === "darwin") return "libllmux.dylib";
  if (Deno.build.os === "windows") return "llmux.dll";
  return "libllmux.so";
}

function goArch(): string {
  return Deno.build.arch === "aarch64" ? "arm64" : "amd64";
}

/**
 * Where the shared library will be loaded from: `LLMUX_LIB`, else a repo
 * checkout's `dist/ffi/<goos>_<goarch>/`, else the bare name for the system
 * loader.
 *
 * Reading `LLMUX_LIB` needs `--allow-env=LLMUX_LIB`; without it the env lookup
 * is skipped rather than throwing, so `--allow-ffi` alone still works against a
 * checkout.
 *
 * There is no prebuilt library for windows/amd64 or darwin/amd64. This function
 * will still name one; `Deno.dlopen` then fails with the loader's own message.
 */
export function resolveLibrary(explicit?: string): string {
  if (explicit) return explicit;
  const fromEnv = Deno.permissions.querySync({ name: "env", variable: "LLMUX_LIB" }).state === "granted"
    ? Deno.env.get("LLMUX_LIB")
    : undefined;
  if (fromEnv) return fromEnv;
  const candidate = new URL(
    `../../dist/ffi/${Deno.build.os}_${goArch()}/${libFileName()}`,
    import.meta.url,
  );
  try {
    Deno.statSync(candidate);
    return candidate.pathname;
  } catch {
    return libFileName();
  }
}

// ---------------------------------------------------------------------------
// The ABI
// ---------------------------------------------------------------------------

// One entry per function in llmux.h, plus a second, `nonblocking` declaration
// of the two that can take a long time. Deno keys symbols by the JS name and
// takes the C name from `name`, which is how llmux_call appears twice.
const SYMBOLS = {
  llmux_abi_version: { parameters: [], result: "pointer" },
  llmux_new: { parameters: ["buffer", "buffer"], result: "u64" },
  llmux_close: { parameters: ["u64"], result: "void" },
  llmux_call: { parameters: ["u64", "buffer", "buffer", "buffer"], result: "pointer" },
  llmux_call_async: {
    name: "llmux_call",
    parameters: ["u64", "buffer", "buffer", "buffer"],
    result: "pointer",
    nonblocking: true,
  },
  llmux_free: { parameters: ["pointer"], result: "void" },
  llmux_stream: {
    parameters: ["u64", "buffer", "buffer", "function", "pointer", "buffer"],
    result: "i32",
    nonblocking: true,
  },
} as const;

type Lib = Deno.DynamicLibrary<typeof SYMBOLS>;

const _libs = new Map<string, Lib>();

function load(libPath: string): Lib {
  const cached = _libs.get(libPath);
  if (cached) return cached;
  const lib = Deno.dlopen(libPath, SYMBOLS);
  _libs.set(libPath, lib);
  return lib;
}

const encoder = new TextEncoder();

/**
 * A NUL-terminated UTF-8 copy of `s`, for a `const char*` parameter.
 *
 * Deliberately backed by a plain ArrayBuffer rather than `encoder.encode`'s
 * `ArrayBufferLike`: Deno's FFI refuses a buffer that might be shared, and it is
 * right to — the library reads it from another thread on a nonblocking call.
 */
function cstr(s: string): Uint8Array<ArrayBuffer> {
  const buf = new Uint8Array(new ArrayBuffer(s.length * 3 + 1)); // 3 bytes/char is UTF-8's worst case here
  const { written } = encoder.encodeInto(s, buf);
  buf[written] = 0;
  return buf.subarray(0, written + 1);
}

/** A `char** err` slot: eight bytes for the library to write a pointer into. */
function errSlot(): BigUint64Array<ArrayBuffer> {
  return new BigUint64Array(new ArrayBuffer(8));
}

function errPointer(slot: BigUint64Array<ArrayBuffer>): Deno.PointerValue {
  return Deno.UnsafePointer.create(slot[0]);
}

/** Read a C string llmux allocated, then free it. Freeing is not optional. */
function takeString(lib: Lib, p: Deno.PointerValue): string | null {
  if (p === null) return null;
  try {
    return Deno.UnsafePointerView.getCString(p);
  } finally {
    lib.symbols.llmux_free(p);
  }
}

/** Turn a populated `char** err` into an Error, freeing the message. */
function takeError(lib: Lib, slot: BigUint64Array<ArrayBuffer>, fallback: string): Error {
  // Error strings are plain UTF-8 text, NOT JSON. Do not parse them.
  const msg = takeString(lib, errPointer(slot));
  slot[0] = 0n;
  return new Error(msg ?? fallback);
}

/** The llmux version the loaded shared library was built from. */
export function abiVersion(libraryPath?: string): string {
  const lib = load(resolveLibrary(libraryPath));
  // Static storage inside the library. Do NOT free it.
  const p = lib.symbols.llmux_abi_version();
  return p === null ? "" : Deno.UnsafePointerView.getCString(p);
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

/**
 * A gateway running in this process.
 *
 * Construction is INERT: no goroutines, no sockets (unless your configuration
 * names a Postgres DSN), no price sync, no spend flusher. Nothing happens until
 * you call.
 *
 * Disposable, so `using gw = Gateway.open()` closes the handle on every exit
 * path out of the block, including a throw.
 */
export class Gateway implements Disposable {
  #lib: Lib;
  #h: bigint;
  #closed = false;

  private constructor(lib: Lib, h: bigint) {
    this.#lib = lib;
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
    const err = errSlot();
    const h = lib.symbols.llmux_new(cfg, err);
    if (h === 0n) throw takeError(lib, err, "llmux_new failed");
    return new Gateway(lib, h);
  }

  /** The registry key of this gateway. Diagnostic only; handles are never reused. */
  get handle(): bigint {
    return this.#h;
  }

  #live(): bigint {
    if (this.#closed) throw new Error("llmux gateway is closed");
    return this.#h;
  }

  #body(request: Record<string, unknown> | string | null | undefined): Uint8Array<ArrayBuffer> | null {
    if (request == null) return null;
    return cstr(typeof request === "string" ? request : JSON.stringify(request));
  }

  /**
   * One unary call, run on THIS thread. Blocks — including the upstream round
   * trip on "chat" — so it stalls the event loop. Fine in a script; prefer
   * {@link call} anywhere that is also serving something else.
   */
  callSync(method: Method, request?: Record<string, unknown> | string | null): unknown {
    const lib = this.#lib;
    const err = errSlot();
    const res = lib.symbols.llmux_call(this.#live(), cstr(method), this.#body(request), err);
    if (res === null) throw takeError(lib, err, `llmux_call(${method}) failed`);
    return JSON.parse(takeString(lib, res) ?? "null");
  }

  /**
   * One unary call, off the event loop.
   *
   * The symbol is declared `nonblocking`, so Deno runs it on a blocking-task
   * thread and resolves a promise when it returns. Nothing else in the isolate
   * has to wait for the provider.
   */
  async call(method: Method, request?: Record<string, unknown> | string | null): Promise<unknown> {
    const lib = this.#lib;
    const err = errSlot();
    const res = await lib.symbols.llmux_call_async(this.#live(), cstr(method), this.#body(request), err);
    if (res === null) throw takeError(lib, err, `llmux_call(${method}) failed`);
    return JSON.parse(takeString(lib, res) ?? "null");
  }

  /**
   * Stream a chat completion as an async iterator.
   *
   * ```ts
   * for await (const chunk of gw.stream({ model, messages })) { ... }
   * ```
   *
   * `break` stops the stream: the C callback returns non-zero, llmux stops at
   * the next chunk boundary, and `llmux_stream` still returns 0. That is not an
   * error — you decided to stop, so llmux does not hand your decision back to
   * you as a failure. Tokens already served are metered either way.
   *
   * How this stays off the event loop: `llmux_stream` is declared `nonblocking`,
   * so it runs on a Deno blocking-task thread while the isolate keeps going, and
   * `Deno.UnsafeCallback` marshals each chunk back in. `chunk_json` is owned by
   * the library and valid only for the duration of the callback, so it is copied
   * into a JS string there and never held as a pointer.
   */
  async *stream(request: Record<string, unknown> | string): AsyncGenerator<ChatChunk, void, unknown> {
    const lib = this.#lib;
    const h = this.#live();
    const body = typeof request === "string"
      ? cstr(request)
      : cstr(JSON.stringify({ ...request, stream: true }));

    const queue: ChatChunk[] = [];
    let wake: (() => void) | null = null;
    let stop = 0;
    let done = false;
    let failure: Error | null = null;

    const ping = () => {
      const w = wake;
      wake = null;
      w?.();
    };

    const cb = new Deno.UnsafeCallback(
      { parameters: ["pointer", "pointer"], result: "i32" } as const,
      (chunkPtr: Deno.PointerValue): number => {
        if (chunkPtr !== null && stop === 0) {
          try {
            queue.push(JSON.parse(Deno.UnsafePointerView.getCString(chunkPtr)) as ChatChunk);
          } catch {
            stop = 1; // malformed chunk: stop rather than loop on garbage
          }
        }
        ping();
        return stop;
      },
    );

    const err = errSlot();
    const call = lib.symbols
      .llmux_stream(h, cstr("chat"), body, cb.pointer, null, err)
      .then((rc) => {
        if (rc !== 0) failure = takeError(lib, err, "llmux_stream failed");
      })
      .catch((e: unknown) => {
        failure = e instanceof Error ? e : new Error("llmux_stream rejected with a non-Error value");
      })
      .finally(() => {
        done = true;
        ping();
      });

    try {
      for (;;) {
        const head = queue.shift();
        if (head !== undefined) {
          yield head;
          continue;
        }
        if (done) break;
        await new Promise<void>((resolve) => {
          wake = resolve;
        });
      }
      if (failure) throw failure;
    } finally {
      // Reached on `break`/`throw` as well as normal completion. Ask llmux to
      // stop, then wait for the native call to unwind BEFORE closing the
      // callback — closing it while the library still holds the pointer is how
      // this becomes a segfault rather than an exception.
      stop = 1;
      await call;
      cb.close();
    }
  }

  /** Release the gateway, aborting any stream still running on it. Idempotent. */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#lib.symbols.llmux_close(this.#h);
    this.#h = 0n;
  }

  [Symbol.dispose](): void {
    this.close();
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

async function freePort(): Promise<number> {
  const l = Deno.listen({ hostname: "127.0.0.1", port: 0 });
  const { port } = l.addr as Deno.NetAddr;
  l.close();
  await Promise.resolve();
  return port;
}

/**
 * `llmux serve` in a child process, started and supervised for you.
 *
 * The same contract every llmux SDK implements: resolve the binary, pick a free
 * loopback port, launch with `LLMUX_ADDR` inheriting the environment so provider
 * keys pass through, poll `/health`, expose the base URL.
 *
 * Async-disposable, so `await using side = await Sidecar.start()` kills the
 * child on the way out of the block.
 */
export class Sidecar implements AsyncDisposable {
  readonly baseURL: string;
  readonly openaiBaseURL: string;
  #child: Deno.ChildProcess;
  #stopped = false;

  private constructor(child: Deno.ChildProcess, baseURL: string) {
    this.#child = child;
    this.baseURL = baseURL;
    this.openaiBaseURL = baseURL + "/v1";
  }

  static async start(opts: SidecarOptions = {}): Promise<Sidecar> {
    const port = opts.port ?? (await freePort());
    const addr = `127.0.0.1:${port}`;
    const binary = opts.binary ?? Deno.env.get("LLMUX_BINARY") ?? "llmux";
    const env: Record<string, string> = { LLMUX_ADDR: addr, ...opts.env };
    if (opts.config) env.LLMUX_CONFIG = opts.config;

    const child = new Deno.Command(binary, { env, stdout: "inherit", stderr: "inherit" }).spawn();
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
        try {
          child.kill();
        } catch {
          // already gone
        }
        throw new Error(`llmux did not become healthy on ${addr} in time`);
      }
      await new Promise((r) => setTimeout(r, 50));
    }
  }

  /** Kill the child and wait for it. Idempotent. */
  async stop(): Promise<void> {
    if (this.#stopped) return;
    this.#stopped = true;
    try {
      this.#child.kill();
    } catch {
      // already exited
    }
    await this.#child.status;
  }

  async [Symbol.asyncDispose](): Promise<void> {
    await this.stop();
  }
}

/**
 * Where the shared library will be loaded from.
 *
 * `LLMUX_LIB` wins. Otherwise a repo checkout's `dist/ffi/<goos>_<goarch>/` is
 * tried — that is the layout `scripts/build-ffi.sh` produces — and failing that
 * the bare name is handed to the system loader (`DYLD_LIBRARY_PATH` /
 * `LD_LIBRARY_PATH` / the default search paths).
 *
 * There are no prebuilt libraries for windows/amd64 or darwin/amd64. See
 * README.md; this function will still name a file that does not exist on those
 * platforms, and `Gateway.open` will fail with the loader's own message.
 */
export declare function resolveLibrary(explicit?: string): string;
/** The llmux version the loaded shared library was built from. */
export declare function abiVersion(libraryPath?: string): string;
export interface GatewayOptions {
    /**
     * An llmux configuration document — the same JSON `llmux serve` reads from
     * llmux.json. Omitted / null means built-in defaults plus the environment
     * (auto-detected providers, LLMUX_* overrides).
     */
    config?: string | Record<string, unknown> | null;
    /** Override the shared library path (otherwise {@link resolveLibrary}). */
    libraryPath?: string;
    /**
     * Refuse to open if the loaded library does not report this version. A stale
     * libllmux earlier on the load path is otherwise called silently, and
     * misbehaves in ways that look like llmux bugs.
     */
    expectVersion?: string;
}
/** One `chat.completion.chunk`, as handed to {@link Gateway.stream}'s callback. */
export interface ChatChunk {
    id?: string;
    object?: string;
    model?: string;
    choices?: {
        index?: number;
        delta?: {
            role?: string;
            content?: string;
        };
        finish_reason?: string | null;
    }[];
    [k: string]: unknown;
}
/**
 * A gateway running in this process.
 *
 * Construction is INERT: no goroutines start, no sockets open (unless your
 * configuration names a Postgres DSN), no background price sync, no spend
 * flusher. Nothing happens until you call.
 *
 * Disposable, so `using gw = Gateway.open()` closes the handle on every exit
 * path out of the block, including a throw.
 */
export declare class Gateway implements Disposable {
    private readonly b;
    private h;
    private closed;
    private constructor();
    static open(opts?: GatewayOptions): Gateway;
    /** The registry key of this gateway. Diagnostic only; handles are never reused. */
    get handle(): number;
    private live;
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
     * BLOCKING. "models" is answered from memory in microseconds; "chat" and
     * "embed" include the whole upstream round trip, so this freezes the event
     * loop for as long as the provider takes. That is the trade this module makes
     * and cannot avoid — see the header comment.
     */
    call(method: "chat" | "embed" | "models", request?: Record<string, unknown> | string | null): unknown;
    /**
     * Stream a chat completion, invoking `onChunk` once per chunk. Returns the
     * number of chunks delivered.
     *
     *   gw.stream({ model, messages }, (c) => {
     *     process.stdout.write(c.choices?.[0]?.delta?.content ?? "");
     *   });
     *
     * Return `false` (or any non-zero number) from `onChunk` to stop early. That
     * stops the stream at the next chunk boundary and is NOT an error: llmux_stream
     * still returns 0. Tokens already served are metered either way.
     *
     * This is a callback, not an async iterator, and that is deliberate. The C
     * callback runs on the calling thread while llmux_stream blocks it, so an
     * async iterator could only be built by moving the call to another thread —
     * which Node cannot do here (see the header) — or by buffering the whole
     * answer and replaying it as fake chunks, which would make time-to-first-token
     * a lie. If you want `for await (const chunk of ...)`, use the sidecar.
     *
     * `opts.signal` is the idiomatic way to cancel a stream from Node: it wires
     * an `AbortSignal` to {@link cancel} so `controller.abort()` reaches
     * llmux_cancel instead of leaving you to poke at the gateway directly. Three
     * things about it are load-bearing, and all three are measured in
     * ../README.md's Cancellation section rather than asserted here:
     *
     *   - A signal that is ALREADY aborted when you call `stream` throws before
     *     anything native happens — no call is started just to be cancelled.
     *   - Calling `controller.abort()` from INSIDE `onChunk` is the case that
     *     matters on Node: the `abort` event fires synchronously, so it reaches
     *     llmux_cancel on the same call stack, before `onChunk` returns and
     *     before llmux_stream's blocking call unwinds. This is the only place a
     *     single-threaded host can cancel from while the call is in flight, and
     *     it is verified safe — it does not deadlock, unlike calling
     *     {@link close} from a callback.
     *   - A signal armed from a `setTimeout` or fired by something outside this
     *     call CANNOT take effect until llmux_stream returns control to the
     *     event loop, because that call blocks the loop for its entire
     *     duration. This is not a bug to route around; it is what "synchronous"
     *     means, and pretending otherwise would be exactly the kind of hollow
     *     guarantee this module exists to avoid.
     *
     * `cancel()` — and therefore this signal — is per-HANDLE, not per-stream: it
     * aborts every call in flight on this gateway. A signal that should only
     * touch this one stream needs its own gateway.
     */
    stream(request: Record<string, unknown> | string, onChunk: (chunk: ChatChunk) => boolean | number | undefined, opts?: {
        signal?: AbortSignal;
    }): number;
    /**
     * Aborts every call in flight on this handle, without closing it: the
     * handle stays open and the next call starts on a fresh context. This is
     * `llmux_cancel` — see ffi/include/llmux.h for the authoritative contract.
     *
     * Safe from another thread and, unlike {@link close}, safe from INSIDE a
     * chunk callback: close() must never be called from a callback because it
     * waits (up to a few seconds) for the very call running that callback,
     * which is a deadlock. cancel() returns immediately either way — it asks
     * the blocked llmux_call/llmux_stream to unwind, it does not wait for it.
     *
     * A no-op if this Gateway is already closed, if nothing is running on the
     * handle, or if the handle is unknown — cancelling twice, or cancelling a
     * gateway that finished on its own a moment earlier, is not an error.
     *
     * PER-HANDLE, NOT PER-CALL. This aborts every call in flight on this
     * gateway, including ones you were not thinking about — a second stream
     * started concurrently on the same handle dies too. If you need to cancel
     * one stream without touching another, give them different gateways: one
     * handle per cancellation scope.
     */
    cancel(): void;
    /** Release the gateway, aborting any stream still running on it. Idempotent. */
    close(): void;
    [Symbol.dispose](): void;
}

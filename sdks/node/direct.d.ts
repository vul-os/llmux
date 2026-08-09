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
     */
    stream(request: Record<string, unknown> | string, onChunk: (chunk: ChatChunk) => boolean | number | undefined): number;
    /** Release the gateway, aborting any stream still running on it. Idempotent. */
    close(): void;
    [Symbol.dispose](): void;
}

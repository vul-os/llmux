"use strict";
// llmux, direct — the gateway running INSIDE your Node process, over the C ABI
// in ffi/include/llmux.h.
//
//   import { Gateway } from "llmux/direct";
//
//   using gw = Gateway.open();               // no port, no child process
//   const answer = gw.call("chat", { model: "gpt-4o-mini", messages: [...] });
//
// READ THIS BEFORE YOU USE IT. Every call here is SYNCHRONOUS and BLOCKS THE
// EVENT LOOP, streaming included, and on Node that is not a limitation we can
// engineer around: a Node thread that has called into a Go c-shared library
// never terminates, so neither a worker_thread nor koffi's `.async` pool can be
// used to get the call off the main thread. Measured, with a minimal repro, in
// README.md — "Why direct mode on Node is synchronous".
//
// The consequence: this module is for CLIs, scripts, batch jobs and tests. For
// a server, and for ANY streaming where time-to-first-token matters, use the
// sidecar — `import { start } from "llmux"` — whose SSE stream is a socket and
// costs the event loop nothing. That is the recommendation, not a hedge.
//
// Everything here is JSON in, JSON out: the SAME JSON the HTTP API uses. A body
// that works against POST /v1/chat/completions works here unchanged.
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
Object.defineProperty(exports, "__esModule", { value: true });
exports.Gateway = void 0;
exports.resolveLibrary = resolveLibrary;
exports.abiVersion = abiVersion;
const fs = __importStar(require("fs"));
const path = __importStar(require("path"));
let _koffi = null;
function koffi() {
    if (_koffi)
        return _koffi;
    try {
        // eslint-disable-next-line @typescript-eslint/no-require-imports -- optional dependency, resolved at first direct-mode use so the sidecar path stays FFI-free
        _koffi = require("koffi");
    }
    catch {
        throw new Error('llmux direct mode needs the optional "koffi" dependency: npm install koffi. ' +
            'Or use the sidecar instead — `require("llmux").start()` — which needs no native FFI.');
    }
    return _koffi;
}
// ---------------------------------------------------------------------------
// Finding libllmux
// ---------------------------------------------------------------------------
function libFileName() {
    if (process.platform === "darwin")
        return "libllmux.dylib";
    if (process.platform === "win32")
        return "llmux.dll";
    return "libllmux.so";
}
function goArch() {
    const map = { arm64: "arm64", x64: "amd64" };
    return map[process.arch] ?? process.arch;
}
function goOS() {
    const map = { darwin: "darwin", linux: "linux", win32: "windows" };
    return map[process.platform] ?? process.platform;
}
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
function resolveLibrary(explicit) {
    if (explicit)
        return explicit;
    if (process.env.LLMUX_LIB)
        return process.env.LLMUX_LIB;
    const candidate = path.join(__dirname, "..", "..", "dist", "ffi", `${goOS()}_${goArch()}`, libFileName());
    if (fs.existsSync(candidate))
        return candidate;
    return libFileName();
}
const _bindings = new Map();
function bind(libPath) {
    const cached = _bindings.get(libPath);
    if (cached)
        return cached;
    const k = koffi();
    const lib = k.load(libPath);
    // Mirrors llmux_chunk_cb in ffi/include/llmux.h exactly.
    const ChunkCb = k.proto("int llmux_chunk_cb(const char *chunk_json, void *user_data)");
    const b = {
        path: libPath,
        // A `const char*` result is decoded to a JS string by koffi and NOT freed.
        // Correct here, and ONLY here: llmux_abi_version returns a static string.
        abi_version: lib.func("const char *llmux_abi_version()"),
        new_: lib.func("uint64_t llmux_new(const char *config_json, _Out_ void **err)"),
        close: lib.func("void llmux_close(uint64_t h)"),
        // Declared `void*`, not `char*`, ON PURPOSE: koffi would decode a `char*`
        // result into a JS string and throw the pointer away, leaving nothing to
        // hand llmux_free. That is the leak this line prevents.
        call: lib.func("void *llmux_call(uint64_t h, const char *method, const char *request_json, _Out_ void **err)"),
        free: lib.func("void llmux_free(void *p)"),
        stream: lib.func("int llmux_stream(uint64_t h, const char *method, const char *request_json, llmux_chunk_cb *cb, void *user_data, _Out_ void **err)"),
        chunkCbPtr: k.pointer(ChunkCb),
    };
    _bindings.set(libPath, b);
    return b;
}
/** The llmux version the loaded shared library was built from. */
function abiVersion(libraryPath) {
    return bind(resolveLibrary(libraryPath)).abi_version();
}
// ---------------------------------------------------------------------------
// Strings the library owns
// ---------------------------------------------------------------------------
/** Read a C string llmux allocated, then free it. Freeing is not optional. */
function takeString(b, p) {
    if (!p)
        return null;
    try {
        return koffi().decode(p, "char", -1);
    }
    finally {
        b.free(p);
    }
}
/** Turn a populated `char** err` into an Error, freeing the message. */
function takeError(b, err, fallback) {
    // Error strings are plain UTF-8 text, NOT JSON. Do not parse them.
    const msg = takeString(b, err[0]);
    err[0] = null;
    return new Error(msg ?? fallback);
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
class Gateway {
    constructor(b, h) {
        this.closed = false;
        this.b = b;
        this.h = h;
    }
    static open(opts = {}) {
        const libPath = resolveLibrary(opts.libraryPath);
        const b = bind(libPath);
        if (opts.expectVersion) {
            const got = b.abi_version();
            if (got !== opts.expectVersion) {
                throw new Error(`libllmux at ${libPath} reports version ${got}, expected ${opts.expectVersion} — ` +
                    "a stale library earlier on the load path is the usual cause");
            }
        }
        const cfg = opts.config == null ? null : typeof opts.config === "string" ? opts.config : JSON.stringify(opts.config);
        const err = [null];
        const h = b.new_(cfg, err);
        if (h === 0)
            throw takeError(b, err, "llmux_new failed");
        return new Gateway(b, h);
    }
    /** The registry key of this gateway. Diagnostic only; handles are never reused. */
    get handle() {
        return this.h;
    }
    live() {
        if (this.closed)
            throw new Error("llmux gateway is closed");
        return this.h;
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
     * BLOCKING. "models" is answered from memory in microseconds; "chat" and
     * "embed" include the whole upstream round trip, so this freezes the event
     * loop for as long as the provider takes. That is the trade this module makes
     * and cannot avoid — see the header comment.
     */
    call(method, request) {
        const b = this.b;
        const err = [null];
        const body = request == null ? null : typeof request === "string" ? request : JSON.stringify(request);
        const res = b.call(this.live(), method, body, err);
        if (!res)
            throw takeError(b, err, `llmux_call(${method}) failed`);
        return JSON.parse(takeString(b, res) ?? "null");
    }
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
    stream(request, onChunk) {
        const b = this.b;
        const h = this.live();
        const body = typeof request === "string" ? request : JSON.stringify({ ...request, stream: true });
        const k = koffi();
        let count = 0;
        let thrown = null;
        const cb = k.register((chunkJson) => {
            // chunk_json is owned by the library and valid only for this call; koffi
            // has already copied it into a JS string, so nothing survives the return.
            if (thrown !== null)
                return 1;
            count++;
            try {
                const verdict = onChunk(JSON.parse(chunkJson));
                if (verdict === false)
                    return 1;
                return typeof verdict === "number" ? verdict : 0;
            }
            catch (e) {
                // An exception must not unwind through a Go call frame. Remember it,
                // ask llmux to stop, and rethrow once llmux_stream has returned.
                thrown = e;
                return 1;
            }
        }, b.chunkCbPtr);
        const err = [null];
        try {
            const rc = b.stream(h, "chat", body, cb, null, err);
            // eslint-disable-next-line @typescript-eslint/only-throw-error -- rethrowing the caller's OWN exception verbatim; wrapping it in an Error would hide their type and stack
            if (thrown !== null)
                throw thrown;
            if (rc !== 0)
                throw takeError(b, err, "llmux_stream failed");
            return count;
        }
        finally {
            // Unregister only after the native call has unwound. Doing it while the
            // library still holds the pointer is how this turns into a crash.
            k.unregister(cb);
            if (err[0])
                b.free(err[0]);
        }
    }
    /** Release the gateway, aborting any stream still running on it. Idempotent. */
    close() {
        if (this.closed)
            return;
        this.closed = true;
        this.b.close(this.h);
        this.h = 0;
    }
    [Symbol.dispose]() {
        this.close();
    }
}
exports.Gateway = Gateway;

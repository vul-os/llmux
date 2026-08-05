"use strict";
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
exports.start = start;
exports.baseURL = baseURL;
exports.openaiBaseURL = openaiBaseURL;
exports.stop = stop;
exports.OpenAI = OpenAI;
// llmux — the LLM multiplexer, embedded locally for Node.
//
//   const llmux = require("llmux");
//   const client = await llmux.OpenAI();   // spawns the gateway, returns OpenAI client
//   const r = await client.chat.completions.create({
//     model: "anthropic/claude-3-5-sonnet",
//     messages: [{ role: "user", content: "hi" }],
//   });
//
// No server to run: the gateway starts as a local child process and your
// existing OpenAI client points at it. Provider keys come from env vars.
const child_process_1 = require("child_process");
const net = __importStar(require("net"));
const http = __importStar(require("http"));
const path = __importStar(require("path"));
const fs = __importStar(require("fs"));
let _proc = null;
let _base = null;
function binaryPath() {
    if (process.env.LLMUX_BINARY)
        return process.env.LLMUX_BINARY;
    const name = process.platform === "win32" ? "llmux.exe" : "llmux";
    const bundled = path.join(__dirname, "bin", name);
    if (fs.existsSync(bundled))
        return bundled;
    return "llmux"; // fall back to PATH
}
function freePort() {
    return new Promise((resolve, reject) => {
        const srv = net.createServer();
        srv.unref();
        srv.on("error", reject);
        srv.listen(0, "127.0.0.1", () => {
            // We just bound to a loopback host:port (not a pipe), so address()
            // is always an AddressInfo here, never a string or null.
            const port = srv.address().port;
            srv.close(() => { resolve(port); });
        });
    });
}
function waitHealthy(base, timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    return new Promise((resolve, reject) => {
        const tick = () => {
            const req = http.get(base + "/health", (res) => {
                res.resume();
                if (res.statusCode === 200) {
                    resolve();
                    return;
                }
                retry();
            });
            req.on("error", retry);
        };
        const retry = () => {
            if (Date.now() > deadline) {
                reject(new Error("llmux did not become healthy in time"));
                return;
            }
            setTimeout(tick, 50);
        };
        tick();
    });
}
/** Start the sidecar (idempotent). Returns the base URL (http://host:port). */
async function start(opts = {}) {
    if (_proc && _proc.exitCode === null)
        return _base;
    const port = opts.port || (await freePort());
    const addr = `127.0.0.1:${String(port)}`;
    const env = Object.assign({}, process.env, { LLMUX_ADDR: addr });
    if (opts.config)
        env.LLMUX_CONFIG = opts.config;
    Object.assign(env, opts.env || {});
    _proc = (0, child_process_1.spawn)(binaryPath(), [], { env, stdio: "inherit" });
    _proc.on("exit", () => { if (_proc && _proc.exitCode !== null)
        _proc = null; });
    // Capture spawn failures (e.g. ENOENT for a missing binary) so they surface
    // as a rejected start() rather than an uncaught 'error' event.
    let spawnError = null;
    _proc.on("error", (e) => { spawnError = e; });
    // spawnError is reassigned inside the "error" listener closure above; read
    // it through a function with an explicit return type rather than the raw
    // variable, since TS's control-flow analysis can't see into the closure
    // and would otherwise (wrongly) treat every read as still `null`.
    const currentSpawnError = () => spawnError;
    _base = `http://${addr}`;
    try {
        const startError = currentSpawnError();
        if (startError)
            throw startError;
        await waitHealthy(_base, opts.timeoutMs || 10000);
    }
    catch (e) {
        stop();
        const failError = currentSpawnError();
        if (failError)
            throw failError;
        throw e instanceof Error ? e : new Error(String(e));
    }
    return _base;
}
/** The running base URL (http://host:port), starting the sidecar if needed. */
async function baseURL() {
    if (_proc && _proc.exitCode === null)
        return _base;
    return start();
}
/** The OpenAI-style base URL (…/v1). */
async function openaiBaseURL() {
    return (await baseURL()) + "/v1";
}
/** Stop the sidecar if running. */
function stop() {
    if (_proc && _proc.exitCode === null)
        _proc.kill();
    _proc = null;
}
// "openai" is an optional peer dependency (see package.json), not installed
// as a devDependency here, so it has no first-party types available to
// import against — its export shape is genuinely unknown to us. Narrow it
// structurally instead of asserting `any`.
function pickOpenAIConstructor(mod) {
    const record = typeof mod === "object" && mod !== null ? mod : {};
    const candidate = record.OpenAI ?? record.default ?? mod;
    if (typeof candidate !== "function") {
        throw new TypeError('the "openai" package did not export a usable constructor');
    }
    return candidate;
}
/** Construct an `openai` client pointed at the local gateway. */
async function OpenAI(opts = {}) {
    // FIXED DEFECT (previously reported, not fixed): opts used to be spread
    // last, so a caller-supplied opts.baseURL silently overrode the gateway
    // URL below and traffic went straight to the provider with no warning —
    // defeating the whole point of this helper. Reject it outright instead of
    // silently dropping or silently honoring it, and do so before the dynamic
    // require() so this check doesn't depend on "openai" being installed.
    if ("baseURL" in opts) {
        throw new Error('llmux.OpenAI() always routes through the local llmux gateway, so a caller-supplied "baseURL" is not allowed ' +
            "(it would silently bypass the gateway). To talk to a provider directly, construct your own `openai` client " +
            'instead of using this helper, e.g. `new (require("openai").OpenAI)({ baseURL: ... })`.');
    }
    // eslint-disable-next-line @typescript-eslint/no-require-imports -- optional peer dep resolved dynamically at runtime; not a static import target (see comment above)
    const openaiModule = require("openai");
    const Ctor = pickOpenAIConstructor(openaiModule);
    const baseUrl = await openaiBaseURL();
    // opts is spread first now, so nothing in it can clobber the gateway
    // baseURL or the default apiKey set below (apiKey remains intentionally
    // overridable: opts.apiKey, when supplied, is exactly what wins here).
    return new Ctor({ ...opts, baseURL: baseUrl, apiKey: opts.apiKey || "llmux-local" });
}
process.on("exit", stop);
process.on("SIGINT", () => { stop(); process.exit(130); });

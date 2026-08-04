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
            srv.close(() => resolve(port));
        });
    });
}
function waitHealthy(base, timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    return new Promise((resolve, reject) => {
        const tick = () => {
            const req = http.get(base + "/health", (res) => {
                res.resume();
                if (res.statusCode === 200)
                    return resolve();
                retry();
            });
            req.on("error", retry);
        };
        const retry = () => {
            if (Date.now() > deadline)
                return reject(new Error("llmux did not become healthy in time"));
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
    const addr = `127.0.0.1:${port}`;
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
    _base = `http://${addr}`;
    try {
        if (spawnError)
            throw spawnError;
        await waitHealthy(_base, opts.timeoutMs || 10000);
    }
    catch (e) {
        stop();
        throw spawnError || e;
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
/** Construct an `openai` client pointed at the local gateway. */
async function OpenAI(opts = {}) {
    // "openai" is an optional peer dependency (see package.json) with no
    // first-party types available here; its export shape is unknown to us.
    const OpenAILib = require("openai");
    const Ctor = OpenAILib.OpenAI || OpenAILib.default || OpenAILib;
    const baseUrl = await openaiBaseURL();
    return new Ctor({ baseURL: baseUrl, apiKey: opts.apiKey || "llmux-local", ...opts });
}
process.on("exit", stop);
process.on("SIGINT", () => { stop(); process.exit(130); });

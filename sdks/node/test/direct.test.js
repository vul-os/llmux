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
var __addDisposableResource = (this && this.__addDisposableResource) || function (env, value, async) {
    if (value !== null && value !== void 0) {
        if (typeof value !== "object" && typeof value !== "function") throw new TypeError("Object expected.");
        var dispose, inner;
        if (async) {
            if (!Symbol.asyncDispose) throw new TypeError("Symbol.asyncDispose is not defined.");
            dispose = value[Symbol.asyncDispose];
        }
        if (dispose === void 0) {
            if (!Symbol.dispose) throw new TypeError("Symbol.dispose is not defined.");
            dispose = value[Symbol.dispose];
            if (async) inner = dispose;
        }
        if (typeof dispose !== "function") throw new TypeError("Object not disposable.");
        if (inner) dispose = function() { try { inner.call(this); } catch (e) { return Promise.reject(e); } };
        env.stack.push({ value: value, dispose: dispose, async: async });
    }
    else if (async) {
        env.stack.push({ async: true });
    }
    return value;
};
var __disposeResources = (this && this.__disposeResources) || (function (SuppressedError) {
    return function (env) {
        function fail(e) {
            env.error = env.hasError ? new SuppressedError(e, env.error, "An error was suppressed during disposal.") : e;
            env.hasError = true;
        }
        var r, s = 0;
        function next() {
            while (r = env.stack.pop()) {
                try {
                    if (!r.async && s === 1) return s = 0, env.stack.push(r), Promise.resolve().then(next);
                    if (r.dispose) {
                        var result = r.dispose.call(r.value);
                        if (r.async) return s |= 2, Promise.resolve(result).then(next, function(e) { fail(e); return next(); });
                    }
                    else s |= 1;
                }
                catch (e) {
                    fail(e);
                }
            }
            if (s === 1) return env.hasError ? Promise.reject(env.error) : Promise.resolve();
            if (env.hasError) throw env.error;
        }
        return next();
    };
})(typeof SuppressedError === "function" ? SuppressedError : function (error, suppressed, message) {
    var e = new Error(message);
    return e.name = "SuppressedError", e.error = error, e.suppressed = suppressed, e;
});
var __importDefault = (this && this.__importDefault) || function (mod) {
    return (mod && mod.__esModule) ? mod : { "default": mod };
};
Object.defineProperty(exports, "__esModule", { value: true });
// Tests for llmux direct mode's cancellation surface.
// Run from sdks/node:  npm test   (builds the TS sources, then runs this
// against the compiled output — see package.json).
//
// Gated on the real libllmux shared library (see `direct` below): direct mode
// is FFI, and there is no fake to substitute for it the way test/sidecar.test.ts
// fakes the llmux binary — a mock koffi binding would only prove the mock
// agrees with itself, not that llmux_cancel does what ffi/include/llmux.h says.
// Build it with ../../scripts/build-ffi.sh if this suite reports it skipped.
//
// Covers the claims in ../README.md's Cancellation section:
//   - a signal already aborted when stream() is called throws before any
//     native call happens;
//   - aborting from INSIDE the chunk callback reaches llmux_cancel and the
//     upstream really does stop generating (checked via fake-upstream.mjs's
//     /generated counter, not just the consumer's own chunk count);
//   - the handle survives a cancellation: a plain call and a fresh stream
//     both work afterwards;
//   - cancel() is a no-op with nothing running, called twice, or called on an
//     already-closed Gateway.
const node_test_1 = require("node:test");
const node_assert_1 = __importDefault(require("node:assert"));
const node_child_process_1 = require("node:child_process");
const readline = __importStar(require("node:readline"));
const path = __importStar(require("node:path"));
const direct_1 = require("../direct");
const HARNESS = path.join(__dirname, "..", "examples", "fake-upstream.mjs");
/**
 * Whether direct mode can run at all in this environment: koffi installed and
 * a libllmux the loader can actually dlopen. abiVersion() is used as the
 * probe rather than checking dist/ffi/<goos>_<goarch>/ by hand, because that
 * duplicates resolveLibrary's own search (LLMUX_LIB, then the repo layout,
 * then the system loader) instead of asking the one function that already
 * knows it.
 */
function directUnavailable() {
    try {
        (0, direct_1.abiVersion)();
        return false;
    }
    catch (e) {
        return `direct mode unavailable: ${e instanceof Error ? e.message : String(e)}`;
    }
}
const skip = directUnavailable();
/** Start examples/fake-upstream.mjs and return its config plus a kill switch. */
async function startHarness(text, delayMs) {
    const child = (0, node_child_process_1.spawn)(process.execPath, [HARNESS], {
        stdio: ["ignore", "pipe", "inherit"],
        env: { ...process.env, FAKE_TEXT: text, FAKE_DELAY_MS: String(delayMs) },
    });
    const rl = readline.createInterface({ input: child.stdout });
    const first = await rl[Symbol.asyncIterator]().next();
    if (first.done)
        throw new Error("fake upstream exited before printing its config");
    const config = first.value.slice("CONFIG ".length);
    const cfg = JSON.parse(config);
    const baseUrl = cfg.providers[0]?.base_url;
    if (!baseUrl)
        throw new Error("fake upstream config had no providers[0].base_url");
    const base = baseUrl.endsWith("/v1") ? baseUrl.slice(0, -"/v1".length) : baseUrl;
    return { config, base, stop: () => void child.kill() };
}
async function generated(base) {
    const res = await fetch(`${base}/generated`);
    const stats = (await res.json());
    return stats.generated;
}
// --- pre-aborted signal ------------------------------------------------------
void (0, node_test_1.test)("a signal already aborted when stream() is called throws before any native call runs", { skip }, () => {
    const env_1 = { stack: [], error: void 0, hasError: false };
    try {
        const gw = __addDisposableResource(env_1, direct_1.Gateway.open({}), false);
        const ac = new AbortController();
        ac.abort();
        let called = false;
        node_assert_1.default.throws(() => {
            gw.stream({ model: "demo", messages: [] }, () => {
                called = true;
            }, { signal: ac.signal });
        }, (e) => e instanceof Error && e.name === "AbortError");
        // If the native call had started, "demo" isn't even routable against a
        // gateway opened with no providers — the failure would still happen, but
        // for the wrong reason. The callback never running is what proves this
        // was rejected up front rather than by llmux itself a moment later.
        node_assert_1.default.strictEqual(called, false);
    }
    catch (e_1) {
        env_1.error = e_1;
        env_1.hasError = true;
    }
    finally {
        __disposeResources(env_1);
    }
});
// --- cancel from inside the chunk callback -----------------------------------
void (0, node_test_1.test)("aborting from inside onChunk reaches llmux_cancel and the upstream stops generating", { skip }, async () => {
    const harness = await startHarness("one two three four five six seven eight nine ten", 20);
    try {
        const env_2 = { stack: [], error: void 0, hasError: false };
        try {
            const gw = __addDisposableResource(env_2, direct_1.Gateway.open({ config: harness.config }), false);
            const ac = new AbortController();
            let seen = 0;
            node_assert_1.default.throws(() => {
                gw.stream({ model: "demo", messages: [{ role: "user", content: "hi" }] }, () => {
                    seen++;
                    // AbortSignal's 'abort' event is synchronous, so this call to
                    // ac.abort() reaches Gateway.cancel -> llmux_cancel on the same
                    // call stack, before this callback returns and before
                    // llmux_stream's blocking call unwinds. That is the property
                    // this whole test exists to check.
                    if (seen === 3)
                        ac.abort();
                }, { signal: ac.signal });
            }, /context canceled/);
            // The consumer saw exactly the chunks delivered before the cancel took
            // effect — the callback form has no queue to overrun (see ../README.md,
            // "What break actually stops").
            node_assert_1.default.strictEqual(seen, 3);
            // The number that matters: not what the consumer counted, but what the
            // upstream actually wrote to the socket. Without this check, a binding
            // could stop reading at chunk 3 while llmux and the provider went on
            // for all 10 words plus the closing frame — the exact bug llmux_cancel
            // exists to fix.
            node_assert_1.default.strictEqual(await generated(harness.base), 3);
            // The handle survives: a plain call and a fresh stream both work.
            const models = gw.call("models");
            node_assert_1.default.ok(Array.isArray(models.data));
            let seenAgain = 0;
            const delivered = gw.stream({ model: "demo", messages: [{ role: "user", content: "hi" }] }, () => {
                seenAgain++;
            });
            // 10 words plus the trailing empty-delta "stop" chunk llmux always
            // sends; fake-upstream.mjs emits no usage frame, so this is 11, not 12.
            node_assert_1.default.strictEqual(delivered, 11);
            node_assert_1.default.strictEqual(seenAgain, 11);
            // Cancelling with nothing in flight, and cancelling twice, are no-ops.
            node_assert_1.default.doesNotThrow(() => {
                gw.cancel();
                gw.cancel();
            });
        }
        catch (e_2) {
            env_2.error = e_2;
            env_2.hasError = true;
        }
        finally {
            __disposeResources(env_2);
        }
    }
    finally {
        harness.stop();
    }
});
// --- cancel() on a closed Gateway ---------------------------------------------
void (0, node_test_1.test)("cancel() on an already-closed Gateway is a no-op", { skip }, () => {
    const gw = direct_1.Gateway.open({});
    gw.close();
    node_assert_1.default.doesNotThrow(() => {
        gw.cancel();
    });
});

"use strict";
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

import { test } from "node:test";
import assert from "node:assert";
import { spawn, type ChildProcess } from "node:child_process";
import * as readline from "node:readline";
import * as path from "node:path";

import { Gateway, abiVersion } from "../direct";

const HARNESS = path.join(__dirname, "..", "examples", "fake-upstream.mjs");

/**
 * Whether direct mode can run at all in this environment: koffi installed and
 * a libllmux the loader can actually dlopen. abiVersion() is used as the
 * probe rather than checking dist/ffi/<goos>_<goarch>/ by hand, because that
 * duplicates resolveLibrary's own search (LLMUX_LIB, then the repo layout,
 * then the system loader) instead of asking the one function that already
 * knows it.
 */
function directUnavailable(): string | false {
  try {
    abiVersion();
    return false;
  } catch (e) {
    return `direct mode unavailable: ${e instanceof Error ? e.message : String(e)}`;
  }
}
const skip = directUnavailable();

/** Start examples/fake-upstream.mjs and return its config plus a kill switch. */
async function startHarness(text: string, delayMs: number): Promise<{ config: string; base: string; stop: () => void }> {
  const child: ChildProcess = spawn(process.execPath, [HARNESS], {
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env, FAKE_TEXT: text, FAKE_DELAY_MS: String(delayMs) },
  });
  const rl = readline.createInterface({ input: child.stdout as NodeJS.ReadableStream });
  const first = await rl[Symbol.asyncIterator]().next();
  if (first.done) throw new Error("fake upstream exited before printing its config");
  const config = first.value.slice("CONFIG ".length);
  const cfg = JSON.parse(config) as { providers: { base_url: string }[] };
  const baseUrl = cfg.providers[0]?.base_url;
  if (!baseUrl) throw new Error("fake upstream config had no providers[0].base_url");
  const base = baseUrl.endsWith("/v1") ? baseUrl.slice(0, -"/v1".length) : baseUrl;
  return { config, base, stop: () => void child.kill() };
}

async function generated(base: string): Promise<number> {
  const res = await fetch(`${base}/generated`);
  const stats = (await res.json()) as { generated: number };
  return stats.generated;
}

// --- pre-aborted signal ------------------------------------------------------

void test(
  "a signal already aborted when stream() is called throws before any native call runs",
  { skip },
  () => {
    using gw = Gateway.open({});
    const ac = new AbortController();
    ac.abort();
    let called = false;
    assert.throws(
      () => {
        gw.stream({ model: "demo", messages: [] }, () => {
          called = true;
        }, { signal: ac.signal });
      },
      (e: unknown) => e instanceof Error && e.name === "AbortError"
    );
    // If the native call had started, "demo" isn't even routable against a
    // gateway opened with no providers — the failure would still happen, but
    // for the wrong reason. The callback never running is what proves this
    // was rejected up front rather than by llmux itself a moment later.
    assert.strictEqual(called, false);
  }
);

// --- cancel from inside the chunk callback -----------------------------------

void test(
  "aborting from inside onChunk reaches llmux_cancel and the upstream stops generating",
  { skip },
  async () => {
    const harness = await startHarness("one two three four five six seven eight nine ten", 20);
    try {
      using gw = Gateway.open({ config: harness.config });
      const ac = new AbortController();
      let seen = 0;
      assert.throws(
        () => {
          gw.stream(
            { model: "demo", messages: [{ role: "user", content: "hi" }] },
            () => {
              seen++;
              // AbortSignal's 'abort' event is synchronous, so this call to
              // ac.abort() reaches Gateway.cancel -> llmux_cancel on the same
              // call stack, before this callback returns and before
              // llmux_stream's blocking call unwinds. That is the property
              // this whole test exists to check.
              if (seen === 3) ac.abort();
            },
            { signal: ac.signal }
          );
        },
        /context canceled/
      );
      // The consumer saw exactly the chunks delivered before the cancel took
      // effect — the callback form has no queue to overrun (see ../README.md,
      // "What break actually stops").
      assert.strictEqual(seen, 3);
      // The number that matters: not what the consumer counted, but what the
      // upstream actually wrote to the socket. Without this check, a binding
      // could stop reading at chunk 3 while llmux and the provider went on
      // for all 10 words plus the closing frame — the exact bug llmux_cancel
      // exists to fix.
      assert.strictEqual(await generated(harness.base), 3);

      // The handle survives: a plain call and a fresh stream both work.
      const models = gw.call("models") as { data: unknown[] };
      assert.ok(Array.isArray(models.data));
      let seenAgain = 0;
      const delivered = gw.stream({ model: "demo", messages: [{ role: "user", content: "hi" }] }, () => {
        seenAgain++;
      });
      // 10 words plus the trailing empty-delta "stop" chunk llmux always
      // sends; fake-upstream.mjs emits no usage frame, so this is 11, not 12.
      assert.strictEqual(delivered, 11);
      assert.strictEqual(seenAgain, 11);

      // Cancelling with nothing in flight, and cancelling twice, are no-ops.
      assert.doesNotThrow(() => {
        gw.cancel();
        gw.cancel();
      });
    } finally {
      harness.stop();
    }
  }
);

// --- cancel() on a closed Gateway ---------------------------------------------

void test("cancel() on an already-closed Gateway is a no-op", { skip }, () => {
  const gw = Gateway.open({});
  gw.close();
  assert.doesNotThrow(() => {
    gw.cancel();
  });
});

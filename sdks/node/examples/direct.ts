// llmux DIRECT mode on Node — the gateway inside this process, over the C ABI.
//
//   npm run build && node examples/direct.ts
//
// No API key and no network: the example spawns examples/fake-upstream.mjs,
// which prints an llmux config routing the model "demo" at itself.
//
// Direct mode on Node is synchronous — every call here blocks the event loop,
// which this example measures rather than asserts. Read ../README.md before
// choosing it for a server; examples/sidecar.ts is the one to copy for that.

import { spawn } from "node:child_process";
import * as readline from "node:readline";
import { fileURLToPath } from "node:url";
import * as path from "node:path";

import { Gateway, abiVersion, resolveLibrary } from "../direct.js";

const here = path.dirname(fileURLToPath(import.meta.url));

/** Start the fake upstream and return its llmux config plus a kill function. */
async function startUpstream(
  text = "the quick brown fox jumps over the lazy dog",
  delayMs: number | string = process.env.FAKE_DELAY_MS ?? "40"
): Promise<{ config: string; stop: () => void }> {
  const child = spawn(process.execPath, [path.join(here, "fake-upstream.mjs")], {
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env, FAKE_TEXT: text, FAKE_DELAY_MS: String(delayMs) },
  });
  const rl = readline.createInterface({ input: child.stdout });
  const first = await rl[Symbol.asyncIterator]().next();
  if (first.done) throw new Error("fake upstream exited before printing its config");
  return { config: first.value.slice("CONFIG ".length), stop: () => void child.kill() };
}

/**
 * The base URL fake-upstream.mjs is actually listening on, derived from the
 * CONFIG line it printed rather than tracked separately — that config is the
 * one source of truth every example already reads, and a second copy of the
 * port would just be one more place for the two to drift.
 */
function harnessBaseURL(configJson: string): string {
  const cfg = JSON.parse(configJson) as { providers: { base_url: string }[] };
  const baseUrl = cfg.providers[0]?.base_url;
  if (!baseUrl) throw new Error("fake upstream config had no providers[0].base_url");
  return baseUrl.endsWith("/v1") ? baseUrl.slice(0, -"/v1".length) : baseUrl;
}

/** GET /generated on the harness: chunks actually written to a socket. */
async function generatedCount(harnessBase: string): Promise<{ generated: number; streams: number; disconnects: number }> {
  const res = await fetch(`${harnessBase}/generated`);
  return (await res.json()) as { generated: number; streams: number; disconnects: number };
}

async function main(): Promise<void> {
  console.log(`node       ${process.version} on ${process.platform}/${process.arch}`);
  console.log(`library    ${resolveLibrary()}`);
  console.log(`abi        ${abiVersion()}\n`);

  const upstream = await startUpstream();
  try {
    // `using` closes the handle on every exit path out of this block, including
    // a throw. Without it, an exception between open and close leaks a gateway.
    using gw = Gateway.open({ config: upstream.config, expectVersion: abiVersion() });
    console.log(`handle     ${String(gw.handle)}\n`);

    // ---- models: answered from memory, no upstream involved ----------------
    const models = gw.call("models") as { data: { id: string }[] };
    console.log("models      ", models.data.map((m) => m.id).join(", "));

    // ---- unary chat, and what it costs the event loop ----------------------
    // The upstream fixture sleeps 40 ms per word, so this takes ~400 ms. A 1 ms
    // interval runs across it: the tick count is how much of the event loop
    // survived. It is zero, and the honesty of this module rests on saying so.
    let ticks = 0;
    const timer = setInterval(() => ticks++, 1);
    const startedAt = Date.now();
    const answer = gw.call("chat", {
      model: "demo",
      messages: [{ role: "user", content: "hello" }],
    }) as { choices: { message: { content: string } }[] };
    const elapsed = Date.now() - startedAt;
    clearInterval(timer);
    console.log("chat        ", JSON.stringify(answer.choices[0]?.message.content));
    console.log(`             blocked the event loop for ${String(elapsed)} ms; timer fired ${String(ticks)}x\n`);

    // ---- streaming: one callback per chunk, on this thread -----------------
    process.stdout.write("stream      ");
    const delivered = gw.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] }, (chunk) => {
      process.stdout.write(chunk.choices?.[0]?.delta?.content ?? "");
    });
    console.log(`\n             ${String(delivered)} chunks\n`);

    // ---- stopping early ----------------------------------------------------
    // Returning false returns non-zero from the C callback. llmux stops at the
    // next chunk boundary, llmux_stream still returns 0, and no error is raised:
    // you decided to stop, so llmux does not hand your own decision back to you.
    // The callback form cannot overrun. There is no queue between the library
    // and you, so the number of chunks the C callback delivered IS the number
    // your callback saw — Node's one advantage over the buffered async
    // iterators the Deno and Bun SDKs expose. Both numbers are printed so the
    // claim is checkable rather than asserted.
    let seen = 0;
    const delivered2 = gw.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] }, () => ++seen < 3);
    console.log(`break       consumed ${String(seen)} chunks; the C callback fired ${String(delivered2)}x (10 = the whole answer)`);
    console.log("            no error raised — stopping is your decision, not a failure\n");

    // ---- cancellation: llmux_cancel via AbortSignal -------------------------
    // `break`, above, is a DIFFERENT feature: the consumer's own callback says
    // stop and llmux_stream honours it at the next boundary — an ordinary,
    // successful early exit. This section is about the other case: someone
    // ELSE deciding the call should die — a timeout, a request that was itself
    // cancelled, a supervisor giving up — reaching in from outside and making
    // llmux actually stop generating upstream, not just stop handing you
    // chunks. Before llmux 0.1.5 there was no way to do that without
    // llmux_close, which tears down the whole gateway and every other stream
    // on it.
    //
    // A dedicated upstream: real per-chunk delay and the /generated counter
    // fake-upstream.mjs exposes, both needed to prove the claim rather than
    // assume it. Nothing about the consumer's own chunk count shows the
    // provider stopped — only the upstream knows that, so the upstream is
    // asked.
    const cancelUpstream = await startUpstream("one two three four five six seven eight nine ten", 100);
    try {
      const cancelBase = harnessBaseURL(cancelUpstream.config);

      // Baseline: run to completion, uncancelled, so "N of a possible M" below
      // has a measured M instead of an assumed one. 10 words plus the trailing
      // empty-delta "stop" chunk llmux always sends is 11 chunks; this fixture
      // emits no usage frame (see fake-upstream.mjs), which is why this number
      // is 11 and not the 12 the Python harness reports for the same text.
      using full = Gateway.open({ config: cancelUpstream.config });
      let seenFull = 0;
      const deliveredFull = full.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] }, () => {
        seenFull++;
      });
      const generatedFull = await generatedCount(cancelBase);
      console.log(
        `cancel      baseline:  consumed ${String(seenFull)} chunks (callback fired ${String(deliveredFull)}x), upstream generated ${String(generatedFull.generated)}`
      );

      // Cancelled: an AbortController whose abort() call happens INSIDE the
      // chunk callback — the only place a single-threaded host can reach it
      // while llmux_stream is blocking this thread. The 'abort' event on an
      // AbortSignal fires SYNCHRONOUSLY, so this reaches Gateway.cancel, and
      // therefore llmux_cancel, on the same call stack: before onChunk
      // returns, before llmux_stream's blocking call unwinds. Measured safe —
      // it does not deadlock, unlike calling close() from inside a callback.
      using partial = Gateway.open({ config: cancelUpstream.config });
      const ac = new AbortController();
      let seenPartial = 0;
      try {
        partial.stream(
          { model: "demo", messages: [{ role: "user", content: "hello" }] },
          () => {
            seenPartial++;
            if (seenPartial === 3) ac.abort();
          },
          { signal: ac.signal }
        );
        console.log("cancel      UNEXPECTED: stream() returned normally after cancel");
      } catch (e) {
        // A cancelled stream is a FAILURE return from llmux_stream — rc=-1,
        // *err="context canceled" — unlike returning non-zero from onChunk,
        // which is 0/no-error because that was the caller's own decision
        // handed back as an ordinary stop. Cancellation is llmux noticing the
        // caller lost interest, and it is reported as the error it is.
        //
        // /generated is cumulative for the life of this upstream process —
        // it already counted the 11 chunks the baseline run above produced —
        // so what THIS run generated is the delta since that measurement, not
        // the raw number.
        const generatedAfter = await generatedCount(cancelBase);
        const generatedThisRun = generatedAfter.generated - generatedFull.generated;
        console.log(
          `            cancelled: consumed ${String(seenPartial)} chunks, upstream generated ${String(generatedThisRun)} of ${String(generatedFull.generated)}`
        );
        console.log(`            error: ${e instanceof Error ? e.message : "non-Error thrown"}\n`);
      }

      // ---- the setTimeout trap ----------------------------------------------
      // llmux_stream blocks the event loop for the whole call, so a timer
      // armed BEFORE the call cannot run until the call returns — Node cannot
      // preempt its own main thread to service it. This is not a corner case
      // to work around; it is the direct-mode trade-off this package exists to
      // be honest about. Proof, not assertion: arm a 50 ms abort against a
      // stream that takes over a second and record when the listener actually
      // ran relative to when the stream started.
      using timed = Gateway.open({ config: cancelUpstream.config });
      const tac = new AbortController();
      const t0 = Date.now();
      let firedAt = -1;
      tac.signal.addEventListener("abort", () => {
        firedAt = Date.now() - t0;
      });
      setTimeout(() => {
        tac.abort();
      }, 50);
      let seenTimed = 0;
      const deliveredTimed = timed.stream(
        { model: "demo", messages: [{ role: "user", content: "hello" }] },
        () => {
          seenTimed++;
        },
        { signal: tac.signal }
      );
      const elapsed = Date.now() - t0;
      // Give the timers phase a turn. A bare setImmediate() is NOT enough here
      // — measured: it runs its check-phase callback before libuv gets back
      // to the overdue timers phase when a koffi callback registration is
      // still in the picture, so `firedAt` would read -1 even though the
      // timer is about to fire a tick later. A real (if tiny) setTimeout
      // forces a pass through the timers phase first. Either way, nothing
      // about the numbers above changes: every chunk was already delivered
      // before this line runs — this wait only lets the already-overdue
      // listener get scheduled so this example can report when it ran.
      await new Promise((r) => setTimeout(r, 10));
      console.log(
        `timer       50 ms abort armed against a ${String(elapsed)} ms stream; delivered ${String(seenTimed)} chunks (callback fired ${String(deliveredTimed)}x) unharmed`
      );
      console.log(
        `            listener fired ${firedAt < 0 ? "not yet" : `at ${String(firedAt)} ms`} — after the stream ended, not during it`
      );
      console.log("            a timer cannot preempt the blocked event loop; only an abort from inside onChunk can\n");
    } finally {
      cancelUpstream.stop();
    }

    // ---- the error path ----------------------------------------------------
    try {
      gw.call("chat", { model: "no-such-model", messages: [{ role: "user", content: "x" }] });
      console.log("error       UNEXPECTED: unknown model was accepted");
    } catch (e) {
      // Plain UTF-8 text, not JSON. The string was freed with llmux_free before
      // this line ran — see takeError() in ../direct.ts.
      console.log(`error       ${e instanceof Error ? e.message : "non-Error thrown"}`);
    }

    // ---- streaming refuses to be a unary call ------------------------------
    try {
      gw.call("chat", { model: "demo", messages: [{ role: "user", content: "x" }], stream: true });
      console.log("stream:true UNEXPECTED: accepted by llmux_call");
    } catch (e) {
      console.log(`stream:true ${e instanceof Error ? e.message : "non-Error thrown"}`);
    }

    // ---- a closed handle ---------------------------------------------------
    const doomed = Gateway.open({ config: upstream.config });
    doomed.close();
    doomed.close(); // idempotent, as llmux_close is
    try {
      doomed.call("models");
    } catch (e) {
      console.log(`closed      ${e instanceof Error ? e.message : "non-Error thrown"}`);
    }
  } finally {
    upstream.stop();
  }
}

await main();

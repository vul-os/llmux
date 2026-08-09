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
async function startUpstream(): Promise<{ config: string; stop: () => void }> {
  const child = spawn(process.execPath, [path.join(here, "fake-upstream.mjs")], {
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env, FAKE_TEXT: "the quick brown fox jumps over the lazy dog", FAKE_DELAY_MS: "40" },
  });
  const rl = readline.createInterface({ input: child.stdout });
  const first = await rl[Symbol.asyncIterator]().next();
  if (first.done) throw new Error("fake upstream exited before printing its config");
  return { config: first.value.slice("CONFIG ".length), stop: () => void child.kill() };
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
    let seen = 0;
    gw.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] }, () => ++seen < 3);
    console.log(`break       stopped after ${String(seen)} chunks, no error raised\n`);

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

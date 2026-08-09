// llmux DIRECT mode on Deno — the gateway inside this process, over the C ABI.
//
//   deno task example:direct
//   # or, spelled out:
//   deno run --allow-ffi --allow-run --allow-env --allow-read examples/direct.ts
//
// --allow-ffi is the one that matters; --allow-run/--allow-read are for the
// fake upstream this example spawns so it needs no API key and no network.
// On Deno 1.x add --unstable-ffi. On Deno 2 the FFI API is stable.

import { abiVersion, Gateway, resolveLibrary } from "../mod.ts";

const enc = new TextEncoder();
const out = (s: string) => Deno.stdout.writeSync(enc.encode(s));

/** Start the fake upstream and return its llmux config plus a kill function. */
async function startUpstream(): Promise<{ config: string; stop: () => void }> {
  const child = new Deno.Command(Deno.execPath(), {
    args: ["run", "--allow-net", "--allow-env", new URL("./fake-upstream.mjs", import.meta.url).pathname],
    env: {
      FAKE_TEXT: "the quick brown fox jumps over the lazy dog",
      FAKE_DELAY_MS: Deno.env.get("FAKE_DELAY_MS") ?? "40",
    },
    stdout: "piped",
  }).spawn();
  const reader = child.stdout.getReader();
  const first = await reader.read();
  if (first.done || !first.value) throw new Error("fake upstream exited before printing its config");
  const line = new TextDecoder().decode(first.value).trim();
  reader.releaseLock();
  return {
    config: line.slice("CONFIG ".length),
    stop: () => {
      try {
        child.kill();
      } catch { /* already gone */ }
    },
  };
}

console.log(`deno       ${Deno.version.deno} on ${Deno.build.os}/${Deno.build.arch}`);
console.log(`library    ${resolveLibrary()}`);
console.log(`abi        ${abiVersion()}\n`);

const upstream = await startUpstream();
try {
  // `using` closes the handle on every exit path out of this block, throw
  // included. Without it, an exception between open and close leaks a gateway.
  using gw = Gateway.open({ config: upstream.config, expectVersion: abiVersion() });
  console.log(`handle     ${gw.handle}\n`);

  // ---- models: answered from memory, no upstream involved -------------------
  const models = await gw.call("models") as { data: { id: string }[] };
  console.log("models      ", models.data.map((m) => m.id).join(", "));

  // ---- unary chat, off the event loop ---------------------------------------
  // The fixture takes ~400 ms to answer. A 1 ms interval runs across the call:
  // the tick count is the proof that the isolate kept going. gw.callSync()
  // would run the same call here and freeze it — try swapping them.
  let ticks = 0;
  const timer = setInterval(() => ticks++, 1);
  const started = Date.now();
  const answer = await gw.call("chat", {
    model: "demo",
    messages: [{ role: "user", content: "hello" }],
  }) as { choices: { message: { content: string } }[] };
  const elapsed = Date.now() - started;
  clearInterval(timer);
  console.log("chat        ", JSON.stringify(answer.choices[0]?.message.content));
  console.log(`             took ${elapsed} ms; the event loop ticked ${ticks}x meanwhile\n`);

  // ---- streaming as an async iterator ---------------------------------------
  out("stream      ");
  let chunks = 0;
  let streamTicks = 0;
  const streamTimer = setInterval(() => streamTicks++, 1);
  for await (const chunk of gw.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] })) {
    chunks++;
    out(chunk.choices?.[0]?.delta?.content ?? "");
  }
  clearInterval(streamTimer);
  console.log(`\n             ${chunks} chunks, event loop ticked ${streamTicks}x during the stream\n`);

  // ---- break out early ------------------------------------------------------
  // The generator's finally block sets the stop flag, so the C callback returns
  // non-zero and llmux stops at the next chunk boundary. llmux_stream still
  // returns 0: stopping was your decision, not a failure.
  // MEASURE the overrun rather than assuming cancellation is instant: nothing
  // back-pressures llmux, so chunks that arrived before the stop flag was set
  // were generated and metered whether or not this loop read them.
  let seen = 0;
  const partial = gw.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] });
  for await (const chunk of partial) {
    void chunk;
    if (++seen === 3) break;
  }
  console.log(
    `break       consumed ${seen} chunks; the C callback fired ${partial.nativeChunks}x (10 = the whole answer)`,
  );
  console.log(`            no error raised — stopping is your decision, not a failure\n`);

  // ---- the error path -------------------------------------------------------
  try {
    await gw.call("chat", { model: "no-such-model", messages: [{ role: "user", content: "x" }] });
    console.log("error       UNEXPECTED: unknown model was accepted");
  } catch (e) {
    // Plain UTF-8 text, not JSON, and already freed with llmux_free.
    console.log(`error       ${e instanceof Error ? e.message : "non-Error thrown"}`);
  }

  // ---- streaming refuses to be a unary call ---------------------------------
  try {
    await gw.call("chat", { model: "demo", messages: [{ role: "user", content: "x" }], stream: true });
    console.log("stream:true UNEXPECTED: accepted by llmux_call");
  } catch (e) {
    console.log(`stream:true ${e instanceof Error ? e.message : "non-Error thrown"}`);
  }

  // ---- a closed handle ------------------------------------------------------
  const doomed = Gateway.open({ config: upstream.config });
  doomed.close();
  doomed.close(); // idempotent, as llmux_close is
  try {
    doomed.callSync("models");
  } catch (e) {
    console.log(`closed      ${e instanceof Error ? e.message : "non-Error thrown"}`);
  }
} finally {
  upstream.stop();
}

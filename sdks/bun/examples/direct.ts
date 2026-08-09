// llmux DIRECT mode on Bun — the gateway inside this process, over the C ABI.
//
//   bun run examples/direct.ts
//
// No permission flags: Bun's FFI has no permission model, which is a real
// difference from Deno and worth knowing before you reach for it.
//
// No API key and no network either — the example spawns
// examples/fake-upstream.mjs, which prints an llmux config routing "demo" at
// itself.

import { abiVersion, Gateway, resolveLibrary } from "../index.ts";

const out = (s: string) => process.stdout.write(s);

/** Start the fake upstream and return its llmux config plus a kill function. */
async function startUpstream(): Promise<{ config: string; stop: () => void }> {
  const child = Bun.spawn(
    [process.execPath, "run", new URL("./fake-upstream.mjs", import.meta.url).pathname],
    {
      stdout: "pipe",
      env: { ...process.env, FAKE_TEXT: "the quick brown fox jumps over the lazy dog", FAKE_DELAY_MS: process.env.FAKE_DELAY_MS ?? "40" },
    },
  );
  const first = await child.stdout.getReader().read();
  if (!first.value) throw new Error("fake upstream exited before printing its config");
  return {
    config: new TextDecoder().decode(first.value).trim().slice("CONFIG ".length),
    stop: () => child.kill(),
  };
}

console.log(`bun        ${Bun.version} on ${process.platform}/${process.arch}`);
console.log(`library    ${resolveLibrary()}`);
console.log(`abi        ${abiVersion()}\n`);

const upstream = await startUpstream();
try {
  // `await using` closes the handle on every exit path out of this block,
  // including a throw. Gateway is AsyncDisposable rather than Disposable
  // because tearing down a stream worker is asynchronous.
  await using gw = Gateway.open({ config: upstream.config, expectVersion: abiVersion() });
  console.log(`handle     ${gw.handle}\n`);

  // ---- models: answered from memory, no upstream involved --------------------
  const models = gw.call("models") as { data: { id: string }[] };
  console.log("models      ", models.data.map((m) => m.id).join(", "));

  // ---- unary chat, and what it costs the event loop --------------------------
  // bun:ffi has no async call mode, so this runs on this thread and freezes the
  // loop. The fixture takes ~400 ms; the tick count below is how much of the
  // event loop survived, and it is zero.
  let ticks = 0;
  const timer = setInterval(() => ticks++, 1);
  const started = Date.now();
  const answer = gw.call("chat", {
    model: "demo",
    messages: [{ role: "user", content: "hello" }],
  }) as { choices: { message: { content: string } }[] };
  const elapsed = Date.now() - started;
  clearInterval(timer);
  console.log("chat        ", JSON.stringify(answer.choices[0]?.message.content));
  console.log(`             blocked the event loop for ${elapsed} ms; timer fired ${ticks}x\n`);

  // ---- streaming as an async iterator ----------------------------------------
  // Same library, same handle — but llmux_stream runs on a Worker, so the loop
  // keeps going. Compare the tick count with the line above.
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

  // ---- break out early --------------------------------------------------------
  // The generator's finally block raises the shared stop flag, the C callback
  // returns it, and llmux stops at the next chunk boundary. llmux_stream still
  // returns 0: stopping was your decision, not a failure.
  // MEASURE the overrun rather than assuming cancellation is instant.
  let seen = 0;
  const partial = gw.stream({ model: "demo", messages: [{ role: "user", content: "hello" }] });
  for await (const chunk of partial) {
    void chunk;
    if (++seen === 3) break;
  }
  console.log(`break       consumed ${seen} chunks; the C callback fired ${partial.nativeChunks}x (10 = the whole answer)`);
  console.log("            no error raised — stopping is your decision, not a failure\n");

  // ---- the error path ---------------------------------------------------------
  try {
    gw.call("chat", { model: "no-such-model", messages: [{ role: "user", content: "x" }] });
    console.log("error       UNEXPECTED: unknown model was accepted");
  } catch (e) {
    // Plain UTF-8 text, not JSON, and already freed with llmux_free.
    console.log(`error       ${e instanceof Error ? e.message : "non-Error thrown"}`);
  }

  // ---- streaming refuses to be a unary call ------------------------------------
  try {
    gw.call("chat", { model: "demo", messages: [{ role: "user", content: "x" }], stream: true });
    console.log("stream:true UNEXPECTED: accepted by llmux_call");
  } catch (e) {
    console.log(`stream:true ${e instanceof Error ? e.message : "non-Error thrown"}`);
  }

  // ---- a closed handle ---------------------------------------------------------
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

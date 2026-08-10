// llmux DIRECT mode on Deno — the gateway inside this process, over the C ABI.
//
//   deno task example:direct
//   # or, spelled out:
//   deno run --allow-ffi --allow-run --allow-env --allow-read --allow-net examples/direct.ts
//
// --allow-ffi is the one that matters; --allow-run/--allow-read are for the
// fake upstream this example spawns so it needs no API key and no network.
// --allow-net is for THIS process, not llmux: the cancellation demo below asks
// the fake upstream's own GET /generated for how many chunks it actually wrote
// to a socket, which is a plain fetch() over loopback.
// On Deno 1.x add --unstable-ffi. On Deno 2 the FFI API is stable.

import { abiVersion, Gateway, resolveLibrary } from "../mod.ts";

const enc = new TextEncoder();
const out = (s: string) => Deno.stdout.writeSync(enc.encode(s));

/**
 * Start the fake upstream and return its llmux config, its own base URL (for
 * GET /generated), and a kill function.
 *
 * `delayMs` defaults to `FAKE_DELAY_MS` (itself defaulting to 40) so the rest
 * of this example can keep tuning the event-loop-tick demonstrations with an
 * environment variable, same as before. The cancellation demo below passes an
 * explicit 100 instead, because that is the number its measurement is reported
 * against and it must not silently drift if someone runs the example with
 * FAKE_DELAY_MS set for an unrelated reason.
 */
async function startUpstream(
  delayMs?: number,
): Promise<{ config: string; baseURL: string; stop: () => void }> {
  const child = new Deno.Command(Deno.execPath(), {
    args: ["run", "--allow-net", "--allow-env", new URL("./fake-upstream.mjs", import.meta.url).pathname],
    env: {
      FAKE_TEXT: "the quick brown fox jumps over the lazy dog",
      FAKE_DELAY_MS: String(delayMs ?? Deno.env.get("FAKE_DELAY_MS") ?? "40"),
    },
    stdout: "piped",
  }).spawn();
  const reader = child.stdout.getReader();
  const first = await reader.read();
  if (first.done || !first.value) throw new Error("fake upstream exited before printing its config");
  const line = new TextDecoder().decode(first.value).trim();
  reader.releaseLock();
  const config = line.slice("CONFIG ".length);
  // GET /generated is served by the same http.Server as the OpenAI-shaped
  // /v1/chat/completions route the config points llmux at — one directory up.
  const { base_url } = (JSON.parse(config).providers[0]) as { base_url: string };
  return {
    config,
    baseURL: base_url.replace(/\/v1$/, ""),
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
  // The generator's finally block now reaches llmux_cancel, not just a local
  // stop flag the callback checks at its NEXT invocation — that used to be up
  // to a whole provider chunk-delay away, which is exactly the gap that let a
  // consumer stop reading after 3 chunks while the provider went on to
  // generate a 4th one anyway. llmux_stream still returns 0 for a plain
  // break: stopping was your own decision, not a failure, so llmux does not
  // hand it back to you as one.
  // MEASURE the overrun rather than assuming cancellation is instant: nothing
  // back-pressures llmux, so a chunk that was already in flight when the
  // cancel landed was generated and metered whether or not this loop read it.
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

  // ---- cancel: the idiomatic construct, AbortSignal --------------------------
  // `break` above stops the loop from INSIDE the loop. AbortSignal is for when
  // the decision to stop is made by somebody who is not the one reading
  // chunks — a timeout, a sibling request, a UI cancel button — which is the
  // actual reason JS grew AbortController rather than just telling everyone to
  // `break`. It reaches the same llmux_cancel, with one behavioural
  // difference: a consumer still awaiting a chunk when the signal fires gets
  // an AbortError thrown at it, because that is somebody else's decision
  // arriving asynchronously, not its own — unlike `break`, which never raises.
  //
  // The proof needs a witness outside this process. Consumer-side counts can
  // only say what THIS loop saw; they cannot say whether the provider kept
  // generating, and billing, after this loop stopped looking. The fake
  // upstream counts chunks it actually wrote to a socket and stops counting
  // the instant the client disconnects — GET /generated is that count. This
  // is the harness's 100 ms/chunk configuration, matched to the measurement in
  // README.md's cancellation section: run any faster and the race between
  // "cancel lands" and "next chunk already left the socket" stops being
  // interesting.
  const cancelUpstream = await startUpstream(100);
  try {
    using cancelGw = Gateway.open({ config: cancelUpstream.config, expectVersion: abiVersion() });
    const controller = new AbortController();
    let cancelSeen = 0;
    let cancelError: unknown;
    try {
      const cancelled = cancelGw.stream(
        { model: "demo", messages: [{ role: "user", content: "hello" }] },
        { signal: controller.signal },
      );
      for await (const chunk of cancelled) {
        void chunk;
        cancelSeen++;
        // Note this does NOT `break`: the loop keeps awaiting, exactly like a
        // real consumer who has handed the AbortSignal to something else and
        // is just reading until told to stop. The abort is what ends the
        // loop, not the loop itself.
        if (cancelSeen === 3) controller.abort();
      }
    } catch (e) {
      cancelError = e;
    }
    const generated = await (await fetch(cancelUpstream.baseURL + "/generated")).json() as {
      generated: number;
    };
    console.log(`cancel      consumer saw ${cancelSeen} chunks before AbortSignal fired`);
    console.log(
      `            ${
        cancelError instanceof DOMException ? cancelError.name : "UNEXPECTED: " + String(cancelError)
      } raised on the chunk it was awaiting when the signal fired`,
    );
    console.log(`            upstream generated ${generated.generated} of 10 words total\n`);
  } finally {
    cancelUpstream.stop();
  }

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

// llmux SIDECAR mode on Deno — `llmux serve` in its own process, HTTP over
// loopback, started and supervised by this module.
//
//   deno task example:sidecar
//   # or, spelled out:
//   deno run --allow-run --allow-net --allow-env --allow-read --allow-write examples/sidecar.ts
//
// No --allow-ffi anywhere: there is no native code in this path at all, and no
// Go runtime in this process. It needs the `llmux` binary — set LLMUX_BINARY,
// or build one:
//   GOFLAGS=-mod=mod go build -o /tmp/llmux ../../cmd/llmux

import { Sidecar } from "../mod.ts";

const enc = new TextEncoder();
const out = (s: string) => Deno.stdout.writeSync(enc.encode(s));

async function startUpstream(): Promise<{ configPath: string; stop: () => void }> {
  const child = new Deno.Command(Deno.execPath(), {
    args: ["run", "--allow-net", "--allow-env", new URL("./fake-upstream.mjs", import.meta.url).pathname],
    env: { FAKE_TEXT: "the quick brown fox jumps over the lazy dog", FAKE_DELAY_MS: "40" },
    stdout: "piped",
  }).spawn();
  const reader = child.stdout.getReader();
  const first = await reader.read();
  if (first.done || !first.value) throw new Error("fake upstream exited before printing its config");
  const line = new TextDecoder().decode(first.value).trim();
  reader.releaseLock();
  // `llmux serve` reads its config from a file named by LLMUX_CONFIG, so the
  // JSON the fixture printed has to land on disk somewhere.
  const configPath = Deno.makeTempDirSync({ prefix: "llmux-example-" }) + "/llmux.json";
  Deno.writeTextFileSync(configPath, line.slice("CONFIG ".length));
  return {
    configPath,
    stop: () => {
      try {
        child.kill();
      } catch { /* already gone */ }
    },
  };
}

console.log(`deno       ${Deno.version.deno} on ${Deno.build.os}/${Deno.build.arch}`);

const upstream = await startUpstream();
try {
  // `await using` kills the child on the way out of this block, including on a
  // throw — Sidecar implements Symbol.asyncDispose.
  await using side = await Sidecar.start({ config: upstream.configPath });
  console.log(`sidecar    ${side.baseURL}`);
  console.log(`openai     ${side.openaiBaseURL}\n`);

  const headers = { "content-type": "application/json", authorization: "Bearer llmux-local" };

  // ---- unary ----------------------------------------------------------------
  const res = await fetch(side.openaiBaseURL + "/chat/completions", {
    method: "POST",
    headers,
    body: JSON.stringify({ model: "demo", messages: [{ role: "user", content: "hello" }] }),
  });
  const answer = await res.json() as { choices: { message: { content: string } }[] };
  console.log("chat        ", JSON.stringify(answer.choices[0]?.message.content));

  // ---- streaming: SSE, read as an async iterator -----------------------------
  // No FFI, no callback, no thread question. This is a socket, which is why the
  // sidecar is the easy answer for streaming in every JS runtime.
  out("\nstream      ");
  const sse = await fetch(side.openaiBaseURL + "/chat/completions", {
    method: "POST",
    headers,
    body: JSON.stringify({
      model: "demo",
      messages: [{ role: "user", content: "hello" }],
      stream: true,
    }),
  });
  if (!sse.body) throw new Error("no response body");
  let chunks = 0;
  let buffered = "";
  const decoder = new TextDecoder();
  for await (const bytes of sse.body) {
    buffered += decoder.decode(bytes, { stream: true });
    let nl: number;
    while ((nl = buffered.indexOf("\n\n")) !== -1) {
      const frame = buffered.slice(0, nl).trim();
      buffered = buffered.slice(nl + 2);
      if (!frame.startsWith("data: ")) continue;
      const payload = frame.slice(6);
      if (payload === "[DONE]") continue;
      chunks++;
      const chunk = JSON.parse(payload) as { choices?: { delta?: { content?: string } }[] };
      out(chunk.choices?.[0]?.delta?.content ?? "");
    }
  }
  console.log(`\n             ${chunks} chunks\n`);

  // ---- the error path ---------------------------------------------------------
  const bad = await fetch(side.openaiBaseURL + "/chat/completions", {
    method: "POST",
    headers,
    body: JSON.stringify({ model: "no-such-model", messages: [{ role: "user", content: "x" }] }),
  });
  console.log(`error       HTTP ${bad.status} ${(await bad.text()).trim().slice(0, 110)}`);
} finally {
  upstream.stop();
}

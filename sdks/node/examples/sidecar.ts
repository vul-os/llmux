// llmux SIDECAR mode on Node — `llmux serve` in its own process, HTTP over
// loopback, started and supervised by this package.
//
//   npm run build && node examples/sidecar.ts
//
// This is the default recommendation for anything that serves requests. There
// is no FFI here, no native dependency, and no Go runtime in your process —
// just a child process and an OpenAI-compatible base URL.
//
// It needs the `llmux` binary. Set LLMUX_BINARY, or build one:
//   GOFLAGS=-mod=mod go build -o /tmp/llmux ./cmd/llmux && export LLMUX_BINARY=/tmp/llmux

import { spawn } from "node:child_process";
import * as readline from "node:readline";
import * as fs from "node:fs";
import * as os from "node:os";
import { fileURLToPath } from "node:url";
import * as path from "node:path";

import { start, baseURL, openaiBaseURL, stop } from "../index.js";

const here = path.dirname(fileURLToPath(import.meta.url));

async function startUpstream(): Promise<{ configPath: string; harnessBase: string; stop: () => void }> {
  const child = spawn(process.execPath, [path.join(here, "fake-upstream.mjs")], {
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env, FAKE_TEXT: "the quick brown fox jumps over the lazy dog", FAKE_DELAY_MS: "40" },
  });
  const rl = readline.createInterface({ input: child.stdout });
  const first = await rl[Symbol.asyncIterator]().next();
  if (first.done) throw new Error("fake upstream exited before printing its config");
  const configJson = first.value.slice("CONFIG ".length);
  // `llmux serve` reads its config from a file named by LLMUX_CONFIG, so the
  // JSON the fixture printed has to land on disk somewhere.
  const configPath = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "llmux-example-")), "llmux.json");
  fs.writeFileSync(configPath, configJson);
  // The fixture's OWN base URL — not llmux's — is where /generated lives; the
  // cancellation demo below asks the upstream directly whether it kept
  // writing to the socket after llmux's sidecar (and this process) walked
  // away from it.
  const cfg = JSON.parse(configJson) as { providers: { base_url: string }[] };
  const baseUrl = cfg.providers[0]?.base_url;
  if (!baseUrl) throw new Error("fake upstream config had no providers[0].base_url");
  const harnessBase = baseUrl.endsWith("/v1") ? baseUrl.slice(0, -"/v1".length) : baseUrl;
  return { configPath, harnessBase, stop: () => void child.kill() };
}

/** GET /generated on the harness: chunks actually written to a socket. */
async function generatedCount(harnessBase: string): Promise<number> {
  const res = await fetch(`${harnessBase}/generated`);
  const stats = (await res.json()) as { generated: number };
  return stats.generated;
}

async function main(): Promise<void> {
  console.log(`node       ${process.version} on ${process.platform}/${process.arch}`);

  const upstream = await startUpstream();
  try {
    // start() is lazy, idempotent and singleton: it picks a free loopback port,
    // spawns the binary with LLMUX_ADDR, and polls /health until it answers.
    const base = await start({ config: upstream.configPath });
    console.log(`sidecar    ${base}`);
    console.log(`openai     ${await openaiBaseURL()}\n`);

    // ---- unary -----------------------------------------------------------
    const res = await fetch((await baseURL()) + "/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer llmux-local" },
      body: JSON.stringify({ model: "demo", messages: [{ role: "user", content: "hello" }] }),
    });
    const answer = (await res.json()) as { choices: { message: { content: string } }[] };
    console.log("chat        ", JSON.stringify(answer.choices[0]?.message.content));

    // ---- streaming: SSE, read as an async iterator ------------------------
    // Nothing clever needed. The event loop was never at risk: this is a
    // socket, not a blocking native call, which is exactly why the sidecar is
    // the easy answer for streaming in every JS runtime.
    process.stdout.write("\nstream      ");
    const sse = await fetch((await baseURL()) + "/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer llmux-local" },
      body: JSON.stringify({ model: "demo", messages: [{ role: "user", content: "hello" }], stream: true }),
    });
    if (!sse.body) throw new Error("no response body");
    let chunks = 0;
    let buffered = "";
    for await (const bytes of sse.body) {
      buffered += Buffer.from(bytes).toString("utf8");
      let nl: number;
      while ((nl = buffered.indexOf("\n\n")) !== -1) {
        const frame = buffered.slice(0, nl).trim();
        buffered = buffered.slice(nl + 2);
        if (!frame.startsWith("data: ")) continue;
        const payload = frame.slice(6);
        if (payload === "[DONE]") continue;
        chunks++;
        const chunk = JSON.parse(payload) as { choices?: { delta?: { content?: string } }[] };
        process.stdout.write(chunk.choices?.[0]?.delta?.content ?? "");
      }
    }
    console.log(`\n             ${String(chunks)} chunks\n`);

    // ---- cancellation: AbortSignal, already free here -----------------------
    // There is no llmux-specific wiring for this — `index.ts` never touches
    // streaming at all, so there was nothing to add there. This request is an
    // ordinary `fetch`, so its ordinary `signal` option aborts it exactly like
    // it would against any other HTTP endpoint. Contrast with
    // sdks/node/examples/direct.ts's "timer" section: there, a timer-based
    // abort cannot fire until llmux_stream returns control to the event loop,
    // because that call blocks it for the whole stream. There is no blocking
    // call here, so the timer fires exactly when armed and the connection
    // dies mid-stream instead of after it. fake-upstream.mjs's /generated
    // counter — queried on the upstream itself, not the llmux sidecar in
    // front of it — proves the provider actually stopped rather than just
    // this process giving up on reading.
    const generatedBefore = await generatedCount(upstream.harnessBase);
    const ac = new AbortController();
    setTimeout(() => {
      ac.abort();
    }, 90); // partway through a ~360 ms, 9-word stream at 40 ms/word
    try {
      const cancelled = await fetch((await baseURL()) + "/v1/chat/completions", {
        method: "POST",
        headers: { "content-type": "application/json", authorization: "Bearer llmux-local" },
        body: JSON.stringify({ model: "demo", messages: [{ role: "user", content: "hello" }], stream: true }),
        signal: ac.signal,
      });
      if (!cancelled.body) throw new Error("no response body");
      // Draining is the point; nothing is done with the bytes themselves —
      // the abort is what is expected to cut this loop short.
      const reader = cancelled.body.getReader();
      try {
        for (;;) {
          const { done } = await reader.read();
          if (done) break;
        }
      } finally {
        reader.releaseLock();
      }
      console.log("cancel      UNEXPECTED: stream completed before the 90 ms abort fired");
    } catch (e) {
      const generatedAfter = await generatedCount(upstream.harnessBase);
      console.log(
        `cancel      aborted 90 ms in: upstream generated ${String(generatedAfter - generatedBefore)} more chunk(s) (a full answer is 10)`
      );
      console.log(`            error: ${e instanceof Error ? e.message : String(e)}\n`);
    }

    // ---- the error path ---------------------------------------------------
    const bad = await fetch((await baseURL()) + "/v1/chat/completions", {
      method: "POST",
      headers: { "content-type": "application/json", authorization: "Bearer llmux-local" },
      body: JSON.stringify({ model: "no-such-model", messages: [{ role: "user", content: "x" }] }),
    });
    console.log(`error       HTTP ${String(bad.status)} ${(await bad.text()).trim().slice(0, 120)}`);
  } finally {
    // stop() kills the child. index.ts also wires it to process exit and
    // SIGINT, so a crash between start() and here does not orphan it either.
    stop();
    upstream.stop();
  }
}

await main();

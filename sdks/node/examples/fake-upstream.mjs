// A tiny OpenAI-compatible upstream, so the examples in this directory run with
// no API keys, no network and no accounts.
//
// It prints one line — `CONFIG <llmux configuration JSON>` — and then serves
// until it is killed. The examples spawn it, read that line, and hand the JSON
// straight to llmux. Printing the config here rather than composing it in each
// example is deliberate: otherwise every example carries its own copy of the
// same config literal and they drift.
//
// It also COUNTS the chunks it generates and serves that count at
// `GET /generated`. That number is how the cancellation example proves what it
// claims: a consumer that stops reading after three chunks learns nothing from
// its own side about whether the provider went on generating the other seven
// and billing for them. Only the upstream knows, so the upstream is asked.
// Generation stops the moment the client goes away — `res.destroyed` after a
// cancelled call closes the connection — because a writer that keeps counting
// into a dead socket would report the full ten every time and the measurement
// would be a fiction.
//
// This file is plain .mjs, not TypeScript, because all three runtimes in the
// suite (node, deno, bun) spawn it verbatim and `node:http` works in all three.
// `sdks/fake-upstream.py` answers the same contract for the languages that
// cannot assume a Node on the box.
//
// Usage:  node fake-upstream.mjs
// Env:    FAKE_TEXT     the assistant answer (default "one two three four")
//         FAKE_DELAY_MS per-chunk delay when streaming (default 0)

import http from "node:http";

const TEXT = process.env.FAKE_TEXT || "one two three four";
const DELAY = Number(process.env.FAKE_DELAY_MS || 0);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Chunks actually written to a socket, streams served, and streams the client
// walked away from. Served at GET /generated.
const stats = { generated: 0, streams: 0, disconnects: 0 };

const server = http.createServer((req, res) => {
  if (req.method === "GET" && req.url.startsWith("/generated")) {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify(stats));
    return;
  }
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", async () => {
    const words = TEXT.split(" ");
    if (!/"stream"\s*:\s*true/.test(body)) {
      // A unary answer takes as long as the whole stream would — otherwise the
      // examples that measure event-loop stall would measure nothing.
      if (DELAY) await sleep(DELAY * words.length);
      res.writeHead(200, { "content-type": "application/json" });
      res.end(
        JSON.stringify({
          id: "chatcmpl-fake",
          object: "chat.completion",
          created: Math.floor(Date.now() / 1000),
          model: "upstream-model",
          choices: [{ index: 0, message: { role: "assistant", content: TEXT }, finish_reason: "stop" }],
          usage: { prompt_tokens: 1, completion_tokens: words.length, total_tokens: 1 + words.length },
        }),
      );
      return;
    }
    stats.streams++;
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    // Returns false once the client has gone, so the loop stops generating
    // instead of counting frames into a closed socket.
    const frame = (o) => {
      if (res.destroyed || res.writableEnded) return false;
      res.write(`data: ${JSON.stringify(o)}\n\n`);
      stats.generated++;
      return true;
    };
    for (const word of words) {
      if (DELAY) await sleep(DELAY);
      const alive = frame({
        id: "chatcmpl-fake",
        object: "chat.completion.chunk",
        created: Math.floor(Date.now() / 1000),
        model: "upstream-model",
        choices: [{ index: 0, delta: { content: word + " " }, finish_reason: null }],
      });
      if (!alive) {
        stats.disconnects++;
        return;
      }
    }
    frame({
      id: "chatcmpl-fake",
      object: "chat.completion.chunk",
      created: Math.floor(Date.now() / 1000),
      model: "upstream-model",
      choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
    });
    if (!res.destroyed) {
      res.write("data: [DONE]\n\n");
      res.end();
    }
  });
});

server.listen(0, "127.0.0.1", () => {
  const base = `http://127.0.0.1:${server.address().port}`;
  console.log(
    "CONFIG " +
      JSON.stringify({
        providers: [{ name: "fake", type: "passthrough", base_url: base + "/v1" }],
        routes: [{ model: "demo", provider: "fake", target_model: "upstream-model" }],
      }),
  );
});

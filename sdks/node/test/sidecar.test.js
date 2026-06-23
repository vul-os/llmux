"use strict";
// Tests for the llmux Node sidecar launcher.
// Run from sdks/node:  node --test
//
// Covers: binary resolution, URL formatting, health-poll readiness/timeout,
// singleton/lazy start, cleanup, and an integration test gated on the real
// binary (LLMUX_BINARY or the bundled bin/llmux).

const { test } = require("node:test");
const assert = require("node:assert");
const path = require("node:path");
const fs = require("node:fs");
const os = require("node:os");
const net = require("node:net");
const http = require("node:http");

const PKG = path.join(__dirname, "..");
const INDEX = path.join(PKG, "index.js");
const FAKE = path.join(PKG, "fixtures", "fake-llmux.js");
const NODE = process.execPath;

// Load a *fresh* copy of the singleton module so tests don't share state.
function freshLlmux() {
  delete require.cache[require.resolve(INDEX)];
  return require(INDEX);
}

// Write an executable wrapper that runs the fake fixture under `node`.
function makeFakeBinary(extraEnv = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "llmux-fake-"));
  const wrapper = path.join(dir, "llmux");
  const exports = Object.entries(extraEnv)
    .map(([k, v]) => `export ${k}="${v}"`)
    .join("\n");
  fs.writeFileSync(
    wrapper,
    `#!/bin/sh\n${exports}\nexec "${NODE}" "${FAKE}"\n`,
    { mode: 0o755 }
  );
  return wrapper;
}

function portOf(base) {
  return parseInt(base.split(":").pop(), 10);
}

function portOpen(port) {
  return new Promise((resolve) => {
    const s = net.connect(port, "127.0.0.1");
    s.setTimeout(500);
    s.on("connect", () => {
      s.destroy();
      resolve(true);
    });
    s.on("error", () => resolve(false));
    s.on("timeout", () => {
      s.destroy();
      resolve(false);
    });
  });
}

async function waitPortClosed(port, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (!(await portOpen(port))) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return !(await portOpen(port));
}

function httpGet(url) {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      res.resume();
      resolve(res.statusCode);
    });
    req.on("error", reject);
  });
}

// --- binary resolution -----------------------------------------------------

test("LLMUX_BINARY override is the binary that gets spawned", async () => {
  // Point the override at our fake and confirm the sidecar actually came up
  // through it (health 200), proving the override path was used.
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary();
  try {
    const base = await llmux.start({ timeoutMs: 10000 });
    assert.strictEqual(await httpGet(base + "/health"), 200);
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

test("a bogus LLMUX_BINARY surfaces a clear failure (not silent bundled use)", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = path.join(os.tmpdir(), "nope-" + Date.now());
  try {
    await assert.rejects(() => llmux.start({ timeoutMs: 800 }));
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

// --- URL formatting --------------------------------------------------------

test("openaiBaseURL == baseURL + /v1, base is http://127.0.0.1:<port>", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary();
  try {
    const base = await llmux.baseURL();
    assert.match(base, /^http:\/\/127\.0\.0\.1:\d+$/);
    const v1 = await llmux.openaiBaseURL();
    assert.strictEqual(v1, base + "/v1");
    assert.ok(v1.endsWith("/v1"));
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

// --- health-poll logic -----------------------------------------------------

test("becomes ready when /health returns 200", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary();
  try {
    const base = await llmux.start({ timeoutMs: 10000 });
    assert.strictEqual(await httpGet(base + "/health"), 200);
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

test("times out when /health never returns 200", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary({ FAKE_HEALTH_STATUS: "503" });
  try {
    await assert.rejects(
      () => llmux.start({ timeoutMs: 600 }),
      /did not become healthy/
    );
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

test("times out when the server never listens", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary({ FAKE_NEVER_LISTEN: "1" });
  try {
    await assert.rejects(() => llmux.start({ timeoutMs: 600 }));
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

// --- singleton / lazy start ------------------------------------------------

test("start twice returns same base and does not respawn", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary();
  try {
    const b1 = await llmux.start();
    const b2 = await llmux.start();
    const b3 = await llmux.baseURL();
    assert.strictEqual(b1, b2);
    assert.strictEqual(b1, b3);
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

// --- cleanup ---------------------------------------------------------------

test("stop() kills the child and frees the port", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = makeFakeBinary();
  try {
    const base = await llmux.start();
    const port = portOf(base);
    assert.ok(await portOpen(port), "port should be open while running");
    llmux.stop();
    assert.ok(await waitPortClosed(port, 3000), "port should be freed after stop");
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

// --- integration (gated on the real binary) --------------------------------

const realBin =
  process.env.LLMUX_BINARY ||
  (fs.existsSync(path.join(PKG, "bin", "llmux"))
    ? path.join(PKG, "bin", "llmux")
    : null);

test(
  "integration: real binary serves health and hands back base_url",
  { skip: realBin ? false : "real llmux binary not available" },
  async () => {
    const llmux = freshLlmux();
    process.env.LLMUX_BINARY = realBin;
    try {
      const base = await llmux.start({ timeoutMs: 15000 });
      assert.match(base, /^http:\/\/127\.0\.0\.1:\d+$/);
      assert.strictEqual(await httpGet(base + "/health"), 200);
      assert.ok((await llmux.openaiBaseURL()).endsWith("/v1"));
    } finally {
      llmux.stop();
    }
  }
);

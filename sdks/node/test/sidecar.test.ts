"use strict";
// Tests for the llmux Node sidecar launcher.
// Run from sdks/node:  npm test   (builds the TS sources, then runs this
// against the compiled output — see package.json).
//
// Covers: binary resolution, URL formatting, health-poll readiness/timeout,
// singleton/lazy start, cleanup, and an integration test gated on the real
// binary (LLMUX_BINARY or the bundled bin/llmux).

import { test } from "node:test";
import assert from "node:assert";
import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import net from "node:net";
import http from "node:http";
import type * as Llmux from "../index";

const PKG = path.join(__dirname, "..");
const INDEX = path.join(PKG, "index.js");
const FAKE = path.join(PKG, "fixtures", "fake-llmux.js");
const NODE = process.execPath;

// Load a *fresh* copy of the singleton module so tests don't share state.
function freshLlmux(): typeof Llmux {
  Reflect.deleteProperty(require.cache, require.resolve(INDEX));
  // A static ES import can't be reloaded per test; this dynamic require is
  // paired with the require.cache delete above specifically to get a fresh
  // module instance between tests, and the result is cast to the module's
  // real type rather than left as `any`.
  // eslint-disable-next-line @typescript-eslint/no-require-imports -- dynamic reload, not a static import target
  return require(INDEX) as typeof Llmux;
}

// Write an executable wrapper that runs the fake fixture under `node`.
function makeFakeBinary(extraEnv: Record<string, string> = {}): string {
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

function portOf(base: string): number {
  return parseInt(base.split(":").pop() as string, 10);
}

function portOpen(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const s = net.connect(port, "127.0.0.1");
    s.setTimeout(500);
    s.on("connect", () => {
      s.destroy();
      resolve(true);
    });
    s.on("error", () => { resolve(false); });
    s.on("timeout", () => {
      s.destroy();
      resolve(false);
    });
  });
}

async function waitPortClosed(port: number, timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (!(await portOpen(port))) return true;
    await new Promise((r) => setTimeout(r, 50));
  }
  return !(await portOpen(port));
}

function httpGet(url: string): Promise<number | undefined> {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      res.resume();
      resolve(res.statusCode);
    });
    req.on("error", reject);
  });
}

// --- binary resolution -----------------------------------------------------

void test("LLMUX_BINARY override is the binary that gets spawned", async () => {
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

void test("a bogus LLMUX_BINARY surfaces a clear failure (not silent bundled use)", async () => {
  const llmux = freshLlmux();
  process.env.LLMUX_BINARY = path.join(os.tmpdir(), "nope-" + Date.now().toString());
  try {
    await assert.rejects(() => llmux.start({ timeoutMs: 800 }));
  } finally {
    llmux.stop();
    delete process.env.LLMUX_BINARY;
  }
});

// --- URL formatting --------------------------------------------------------

void test("openaiBaseURL == baseURL + /v1, base is http://127.0.0.1:<port>", async () => {
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

void test("becomes ready when /health returns 200", async () => {
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

void test("times out when /health never returns 200", async () => {
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

void test("times out when the server never listens", async () => {
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

void test("start twice returns same base and does not respawn", async () => {
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

void test("stop() kills the child and frees the port", async () => {
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

void test(
  "integration: real binary serves health and hands back base_url",
  { skip: realBin ? false : "real llmux binary not available" },
  async () => {
    const llmux = freshLlmux();
    process.env.LLMUX_BINARY = realBin as string;
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

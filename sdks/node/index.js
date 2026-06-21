"use strict";
// llmux — the LLM multiplexer, embedded locally for Node.
//
//   const llmux = require("llmux");
//   const client = await llmux.OpenAI();   // spawns the gateway, returns OpenAI client
//   const r = await client.chat.completions.create({
//     model: "anthropic/claude-3-5-sonnet",
//     messages: [{ role: "user", content: "hi" }],
//   });
//
// No server to run: the gateway starts as a local child process and your
// existing OpenAI client points at it. Provider keys come from env vars.

const { spawn } = require("child_process");
const net = require("net");
const http = require("http");
const path = require("path");
const fs = require("fs");
const os = require("os");

let _proc = null;
let _base = null;

function binaryPath() {
  if (process.env.LLMUX_BINARY) return process.env.LLMUX_BINARY;
  const name = process.platform === "win32" ? "llmux.exe" : "llmux";
  const bundled = path.join(__dirname, "bin", name);
  if (fs.existsSync(bundled)) return bundled;
  return "llmux"; // fall back to PATH
}

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const port = srv.address().port;
      srv.close(() => resolve(port));
    });
  });
}

function waitHealthy(base, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const req = http.get(base + "/health", (res) => {
        res.resume();
        if (res.statusCode === 200) return resolve();
        retry();
      });
      req.on("error", retry);
    };
    const retry = () => {
      if (Date.now() > deadline) return reject(new Error("llmux did not become healthy in time"));
      setTimeout(tick, 50);
    };
    tick();
  });
}

/** Start the sidecar (idempotent). Returns the base URL (http://host:port). */
async function start(opts = {}) {
  if (_proc && _proc.exitCode === null) return _base;
  const port = opts.port || (await freePort());
  const addr = `127.0.0.1:${port}`;
  const env = Object.assign({}, process.env, { LLMUX_ADDR: addr });
  if (opts.config) env.LLMUX_CONFIG = opts.config;
  Object.assign(env, opts.env || {});

  _proc = spawn(binaryPath(), [], { env, stdio: "inherit" });
  _proc.on("exit", () => { if (_proc && _proc.exitCode !== null) _proc = null; });
  _base = `http://${addr}`;
  try {
    await waitHealthy(_base, opts.timeoutMs || 10000);
  } catch (e) {
    stop();
    throw e;
  }
  return _base;
}

async function baseURL() {
  if (_proc && _proc.exitCode === null) return _base;
  return start();
}

async function openaiBaseURL() {
  return (await baseURL()) + "/v1";
}

function stop() {
  if (_proc && _proc.exitCode === null) _proc.kill();
  _proc = null;
}

/** Return an `openai` client pointed at the local gateway. Requires `openai`. */
async function OpenAI(opts = {}) {
  const OpenAILib = require("openai");
  const Ctor = OpenAILib.OpenAI || OpenAILib.default || OpenAILib;
  const baseUrl = await openaiBaseURL();
  return new Ctor({ baseURL: baseUrl, apiKey: opts.apiKey || "llmux-local", ...opts });
}

process.on("exit", stop);
process.on("SIGINT", () => { stop(); process.exit(130); });

module.exports = { start, stop, baseURL, openaiBaseURL, OpenAI };

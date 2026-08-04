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

import { spawn, type ChildProcess } from "child_process";
import * as net from "net";
import * as http from "http";
import * as path from "path";
import * as fs from "fs";
// NOTE: the original JS also required "os" (`const os = require("os");`)
// but never referenced it — a pre-existing dead import. Reproducing that
// via a side-effect `import "os";` here would leak into the generated
// .d.ts (an `import "os";` line at module scope), so it is dropped rather
// than carried forward. See report for this as a called-out discrepancy.

/** Options for {@link start}. */
export interface StartOptions {
  /** Fixed port; defaults to an ephemeral free port. */
  port?: number;
  /** Path to a JSON config file. */
  config?: string;
  /** Extra environment variables for the child process. */
  env?: Record<string, string>;
  /** Health-check timeout in milliseconds (default 10000). */
  timeoutMs?: number;
}

let _proc: ChildProcess | null = null;
let _base: string | null = null;

function binaryPath(): string {
  if (process.env.LLMUX_BINARY) return process.env.LLMUX_BINARY;
  const name = process.platform === "win32" ? "llmux.exe" : "llmux";
  const bundled = path.join(__dirname, "bin", name);
  if (fs.existsSync(bundled)) return bundled;
  return "llmux"; // fall back to PATH
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      // We just bound to a loopback host:port (not a pipe), so address()
      // is always an AddressInfo here, never a string or null.
      const port = (srv.address() as net.AddressInfo).port;
      srv.close(() => resolve(port));
    });
  });
}

function waitHealthy(base: string, timeoutMs: number): Promise<void> {
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
export async function start(opts: StartOptions = {}): Promise<string> {
  if (_proc && _proc.exitCode === null) return _base as string;
  const port = opts.port || (await freePort());
  const addr = `127.0.0.1:${port}`;
  const env = Object.assign({}, process.env, { LLMUX_ADDR: addr });
  if (opts.config) env.LLMUX_CONFIG = opts.config;
  Object.assign(env, opts.env || {});

  _proc = spawn(binaryPath(), [], { env, stdio: "inherit" });
  _proc.on("exit", () => { if (_proc && _proc.exitCode !== null) _proc = null; });
  // Capture spawn failures (e.g. ENOENT for a missing binary) so they surface
  // as a rejected start() rather than an uncaught 'error' event.
  let spawnError: Error | null = null;
  _proc.on("error", (e) => { spawnError = e; });
  _base = `http://${addr}`;
  try {
    if (spawnError) throw spawnError;
    await waitHealthy(_base, opts.timeoutMs || 10000);
  } catch (e) {
    stop();
    throw spawnError || e;
  }
  return _base;
}

/** The running base URL (http://host:port), starting the sidecar if needed. */
export async function baseURL(): Promise<string> {
  if (_proc && _proc.exitCode === null) return _base as string;
  return start();
}

/** The OpenAI-style base URL (…/v1). */
export async function openaiBaseURL(): Promise<string> {
  return (await baseURL()) + "/v1";
}

/** Stop the sidecar if running. */
export function stop(): void {
  if (_proc && _proc.exitCode === null) _proc.kill();
  _proc = null;
}

/** Construct an `openai` client pointed at the local gateway. */
export async function OpenAI(opts: { apiKey?: string; [k: string]: unknown } = {}): Promise<unknown> {
  // "openai" is an optional peer dependency (see package.json) with no
  // first-party types available here; its export shape is unknown to us.
  const OpenAILib: any = require("openai");
  const Ctor = OpenAILib.OpenAI || OpenAILib.default || OpenAILib;
  const baseUrl = await openaiBaseURL();
  return new Ctor({ baseURL: baseUrl, apiKey: opts.apiKey || "llmux-local", ...opts });
}

process.on("exit", stop);
process.on("SIGINT", () => { stop(); process.exit(130); });

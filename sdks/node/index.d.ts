// Type definitions for llmux (Node).

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

/** Start the local gateway sidecar (idempotent). Resolves to the base URL. */
export function start(opts?: StartOptions): Promise<string>;

/** Stop the sidecar if running. */
export function stop(): void;

/** The running base URL (http://host:port), starting the sidecar if needed. */
export function baseURL(): Promise<string>;

/** The OpenAI-style base URL (…/v1). */
export function openaiBaseURL(): Promise<string>;

/** Construct an `openai` client pointed at the local gateway. */
export function OpenAI(opts?: { apiKey?: string; [k: string]: unknown }): Promise<unknown>;

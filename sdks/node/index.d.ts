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
/** Start the sidecar (idempotent). Returns the base URL (http://host:port). */
export declare function start(opts?: StartOptions): Promise<string>;
/** The running base URL (http://host:port), starting the sidecar if needed. */
export declare function baseURL(): Promise<string>;
/** The OpenAI-style base URL (…/v1). */
export declare function openaiBaseURL(): Promise<string>;
/** Stop the sidecar if running. */
export declare function stop(): void;
/** Construct an `openai` client pointed at the local gateway. */
export declare function OpenAI(opts?: {
    apiKey?: string;
    [k: string]: unknown;
}): Promise<unknown>;

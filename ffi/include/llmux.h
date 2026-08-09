/*
 * llmux.h — the stable C ABI of libllmux.
 *
 * llmux is an OpenAI-compatible LLM gateway (routing, retries, failover,
 * sovereignty enforcement, BYOK, caching, pricing, metering). Go hosts import
 * github.com/vul-os/llmux/core/gateway directly; every other language either
 * loads this shared library and runs llmux IN ITS OWN PROCESS, or talks to the
 * `llmux serve` sidecar over HTTP.
 *
 * This header is hand-written and is the supported surface. The header cgo
 * generates next to the library (libllmux.h) declares the same six symbols but
 * drags in Go's typedefs and drops the const qualifiers; use this one.
 *
 * BEFORE YOU LOAD THIS LIBRARY, read ffi/README.md. In summary:
 *   - It puts the Go runtime — GC, scheduler, signal handlers — inside your
 *     process. Hosts with their own SIGSEGV/SIGPROF handling (JVMs, some
 *     Python profilers) can conflict.
 *   - It is NOT fork-safe. After fork() without exec() the Go runtime in the
 *     child is broken. Python multiprocessing must use the "spawn" start
 *     method; uWSGI and Unicorn pre-fork models must load it post-fork.
 *   - It is a ~13 MB artifact. A shared library is not free.
 * If any of that is disqualifying for your host, use the sidecar. That is a
 * supported answer, not a fallback.
 *
 * CONVENTIONS
 *
 *   Strings   UTF-8, NUL-terminated.
 *   Documents Requests and responses are JSON — the SAME JSON the HTTP API
 *             uses. A body that works against POST /v1/chat/completions works
 *             here unchanged, and the response is what that endpoint returns.
 *   Errors    Functions that can fail take a trailing `char** err`. On failure
 *             they write a malloc'd, human-readable UTF-8 message there. Pass
 *             NULL for err if you do not want the message. The message is NOT
 *             JSON; do not parse it.
 *   Ownership Every non-const char* this library returns — results AND error
 *             messages — is freed with llmux_free, and with nothing else.
 *             llmux_abi_version is the one exception: it returns a static
 *             string you must NOT free.
 *   Handles   Integers in a registry inside the library, never pointers.
 *             Calling with a closed or invented handle is a clean error, not a
 *             crash. Handles are never reused.
 *   Threads   A handle is safe to use from several threads at once.
 *   Panics    A Go panic inside the library is caught at every entry point and
 *             returned to you as an ordinary error (message + Go stack), never
 *             allowed to escape into your process. A panic that escaped would
 *             not be an exception your language could catch: it is a Go runtime
 *             fatal error and it would take the whole host down.
 *
 * SPDX-License-Identifier: MIT OR Apache-2.0
 */

#ifndef LLMUX_H
#define LLMUX_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Returns the llmux version this library was built from, e.g. "0.1.2", as a
 * static string. Do NOT free it.
 *
 * Compare it against the version your bindings were generated for. A shared
 * library is resolved off a load path you may not control; without this probe a
 * stale libllmux earlier on that path is called silently and misbehaves in ways
 * that look like llmux bugs.
 */
const char* llmux_abi_version(void);

/*
 * Creates a gateway and returns its handle, or 0 on failure with *err set.
 *
 * config_json is an llmux configuration document — the same JSON `llmux serve`
 * reads from llmux.json. NULL or "" means built-in defaults plus the
 * environment (auto-detected providers, LLMUX_* overrides), exactly as a
 * missing config file does.
 *
 * The gateway is INERT: creating it starts no goroutines and — unless your
 * configuration names a Postgres DSN, which connects and migrates eagerly —
 * opens no sockets. There is no background price-catalog sync and no spend
 * flusher in library mode. Nothing happens until you call.
 */
uint64_t llmux_new(const char* config_json, char** err);

/*
 * Releases the gateway behind h, aborting any stream still running on it.
 * Closing an unknown or already-closed handle is a no-op, so cleanup paths can
 * be idempotent.
 */
void llmux_close(uint64_t h);

/*
 * One unary call. Returns malloc'd UTF-8 JSON (free with llmux_free), or NULL
 * with *err set.
 *
 *   method            request_json                    result
 *   ----------------------------------------------------------------------
 *   "chat"            OpenAI chat-completions body    a chat completion
 *   "embed"           OpenAI embeddings body          an embeddings response
 *   "models"          ignored (pass NULL)             an OpenAI model list
 *
 * A "chat" request with "stream": true is REFUSED rather than quietly served
 * as one blob — use llmux_stream.
 *
 * `method` is a string rather than one C function per operation so this header
 * stays stable as llmux grows methods.
 */
char* llmux_call(uint64_t h, const char* method, const char* request_json, char** err);

/*
 * Frees anything this library returned: results and error messages alike.
 * NULL is accepted and ignored.
 */
void llmux_free(char* p);

/*
 * One streaming chunk. Return 0 to keep streaming, non-zero to stop.
 *
 * chunk_json is one `chat.completion.chunk` object — byte for byte what the
 * HTTP API writes after `data: ` in its SSE stream, without the prefix and
 * without the terminal [DONE] frame. It is owned by the library and valid only
 * for the duration of this call: COPY IT if you need it afterwards.
 *
 * The callback runs on the thread that called llmux_stream, synchronously,
 * before llmux_stream returns. (ctest/smoke.c asserts this by comparing thread
 * ids.) It is nevertheless a callback INTO your host from a Go call frame:
 * Python bindings must reacquire the GIL, and JVM bindings must have the thread
 * attached. If your language's FFI cannot take a C callback safely, use the
 * sidecar's SSE stream instead of pretending — see ffi/README.md.
 */
typedef int (*llmux_chunk_cb)(const char* chunk_json, void* user_data);

/*
 * Streams a chat completion, invoking cb once per chunk. Blocks until the
 * stream ends. method must be "chat".
 *
 * Returns 0 on success, -1 on failure with *err set.
 *
 * A callback that returns non-zero stops the stream and is NOT a failure: the
 * return is 0 and *err is untouched. Your callback returned non-zero, so you
 * already know it happened; llmux does not hand you back an error for your own
 * decision. Tokens already served are metered either way.
 */
int llmux_stream(uint64_t h, const char* method, const char* request_json,
                 llmux_chunk_cb cb, void* user_data, char** err);

/*
 * Function-pointer types for hosts that dlopen/LoadLibrary this library rather
 * than linking against it — which is the usual way, since it lets you probe
 * llmux_abi_version before committing to the rest.
 */
typedef const char* (*llmux_abi_version_fn)(void);
typedef uint64_t (*llmux_new_fn)(const char*, char**);
typedef void (*llmux_close_fn)(uint64_t);
typedef char* (*llmux_call_fn)(uint64_t, const char*, const char*, char**);
typedef void (*llmux_free_fn)(char*);
typedef int (*llmux_stream_fn)(uint64_t, const char*, const char*, llmux_chunk_cb, void*, char**);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* LLMUX_H */

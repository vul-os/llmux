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
 * generates next to the library (libllmux.h) declares the same seven symbols but
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
 *   Errors    Functions that can fail take a trailing `char** err`. On entry
 *             they set *err to NULL; on failure they write a malloc'd,
 *             human-readable UTF-8 message there. So *err being non-NULL after
 *             a call always means THAT call failed, and one `char *err = NULL;`
 *             is safe to reuse across calls. Pass NULL for err if you do not
 *             want the message. The message is NOT JSON; do not parse it.
 *             (Clearing does not free what was there — ownership passed to you
 *             when it was written. Free it before you reuse the variable.)
 *   Ownership Every non-const char* this library returns — results AND error
 *             messages — is freed with llmux_free, and with nothing else.
 *             llmux_abi_version is the one exception: it returns the SAME
 *             pointer on every call, allocated once when the library loads and
 *             never freed. You must NOT free it, and freeing it corrupts the
 *             allocator for everything else in your process.
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
 * Returns the llmux version this library was built from, e.g. "0.1.2".
 *
 * The pointer is the same on every call, is owned by the library, and lives as
 * long as the library is loaded. Do NOT free it and do NOT pass it to
 * llmux_free. Copy it if you need to own a string.
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
 * LLMUX_-namespaced environment (auto-detected providers, LLMUX_* overrides).
 *
 * YOUR DOCUMENT WINS OVER THE ENVIRONMENT. A field your document states is
 * never overwritten by an environment variable — including a field you set to
 * "" or 0, which is a statement, not a gap. Variables fill in only what your
 * document is silent about. (The `llmux serve` sidecar resolves the other way
 * round, env last: there the operator owns both the file and the environment.
 * Here the environment belongs to your application, not to llmux.)
 *
 * FOR THE SAME REASON, THIS LIBRARY IGNORES DATABASE_URL AND VULOS_DATABASE_URL.
 * Those are your application's variables — llmux has no claim on them, and
 * adopting one would mean connecting to your database and running
 * CREATE SCHEMA / CREATE TABLE in it because you loaded a shared library.
 * LLMUX_POSTGRES is namespaced and unambiguous, so it is still honoured; to
 * point llmux at a database from a host that sets DATABASE_URL, put the DSN in
 * your document (or copy it into LLMUX_POSTGRES).
 *
 * The gateway is INERT: creating it starts no goroutines and — unless YOUR
 * configuration names a Postgres DSN, which connects and migrates eagerly and
 * is bounded by postgres_connect_timeout_seconds, 30s by default — opens no
 * sockets. There is no background price-catalog sync and no
 * spend flusher in library mode. Nothing happens until you call.
 */
uint64_t llmux_new(const char* config_json, char** err);

/*
 * Releases the gateway behind h. It cancels every call in flight and then WAITS
 * (up to a few seconds) for those calls to return before releasing the Redis
 * client and Postgres pool — closing them underneath a running call would be a
 * use-after-close inside your process.
 *
 * So llmux_close can block briefly, and it must NOT be called from inside a
 * chunk callback: that would be waiting on the very call that is running it.
 * Return from your callback (return non-zero to stop the stream) and close
 * afterwards.
 *
 * Closing an unknown or already-closed handle is a no-op, so cleanup paths can
 * be idempotent.
 */
void llmux_close(uint64_t h);

/*
 * Aborts every call in flight on h, WITHOUT closing it: the handle stays open
 * and the next call starts on a fresh context.
 *
 * llmux_call and llmux_stream block until they finish. This is the only way to
 * abandon one — call it from another thread while the call is blocked and the
 * blocked call returns an error (a cancelled stream that had already delivered
 * chunks returns -1 with *err set; tokens already served are still metered).
 * Cancelling an unknown handle, or one with nothing running, is a no-op.
 *
 * It is NOT a substitute for the liveness bounds llmux applies to a stream on
 * its own: see stream_first_byte_timeout_seconds and stream_idle_timeout_seconds
 * in the configuration document. This is for when YOUR side loses interest.
 */
void llmux_cancel(uint64_t h);

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
 * return is 0 and *err is left NULL. Your callback returned non-zero, so you
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
typedef void (*llmux_cancel_fn)(uint64_t);
typedef char* (*llmux_call_fn)(uint64_t, const char*, const char*, char**);
typedef void (*llmux_free_fn)(char*);
typedef int (*llmux_stream_fn)(uint64_t, const char*, const char*, llmux_chunk_cb, void*, char**);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* LLMUX_H */

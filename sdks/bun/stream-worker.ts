// The worker half of Bun's llmux_stream support. Not a public entry point —
// index.ts spawns it; see the "Streaming" section of README.md for why.
//
// `bun:ffi` has no asynchronous call mode: every dlopen'd symbol runs on the
// thread that calls it, so calling llmux_stream from the main thread would
// freeze the event loop for the whole completion. This file runs it on a worker
// thread instead and posts each chunk back.
//
// The handle is created by the MAIN thread and passed in. That works — and is
// not a trick — because a Bun Worker is a thread in the same process, dlopen of
// the same path resolves to the library already loaded there, and llmux.h says
// "a handle is safe to use from several threads at once". So there is exactly
// one gateway, with one cache and one set of spend counters, not two.

import { parentPort, workerData } from "node:worker_threads";
import { CString, dlopen, FFIType, JSCallback, ptr } from "bun:ffi";
import type { Pointer } from "bun:ffi";

export interface StreamWorkerData {
  libPath: string;
  handle: bigint;
  /** The request body, already JSON-encoded and already carrying "stream": true. */
  request: string;
  /** One Int32: non-zero means the consumer asked to stop. */
  stopFlag: SharedArrayBuffer;
}

const { libPath, handle, request, stopFlag } = workerData as StreamWorkerData;
const stop = new Int32Array(stopFlag);

const { symbols } = dlopen(libPath, {
  llmux_free: { args: [FFIType.ptr], returns: FFIType.void },
  llmux_stream: {
    args: [FFIType.u64, FFIType.cstring, FFIType.cstring, FFIType.function, FFIType.ptr, FFIType.ptr],
    returns: FFIType.i32,
  },
});

const encoder = new TextEncoder();
const cstr = (s: string) => encoder.encode(s + "\0");

// The callback runs on THIS thread, synchronously, inside llmux_stream — so no
// `threadsafe: true` and no cross-thread marshalling is needed. That is what
// ffi/ctest/smoke.c asserts with pthread_self(), rather than assuming it.
const onChunk = new JSCallback(
  (chunkPtr: Pointer): number => {
    // chunk_json is owned by the library and valid only for this call. new
    // CString copies it, so nothing here outlives the return.
    parentPort!.postMessage({ chunk: new CString(chunkPtr).toString() });
    return Atomics.load(stop, 0);
  },
  { args: [FFIType.ptr, FFIType.ptr], returns: FFIType.i32 },
);

const err = new BigUint64Array(1);
let failure: string | null = null;
try {
  const rc = symbols.llmux_stream(handle, cstr("chat"), cstr(request), onChunk.ptr, null, ptr(err));
  if (rc !== 0) {
    // Errors are plain UTF-8 text, not JSON, and are freed with llmux_free like
    // any other string the library returns. The address has to be re-branded as
    // a Pointer after its round trip through the BigUint64Array slot.
    const p = (err[0] ?? 0n) === 0n ? null : (Number(err[0]) as unknown as Pointer);
    failure = p ? new CString(p).toString() : "llmux_stream failed";
    if (p) symbols.llmux_free(p);
  }
} catch (e) {
  failure = e instanceof Error ? e.message : "llmux_stream threw a non-Error value";
} finally {
  // Close the callback only after llmux_stream has unwound; the library holds
  // the pointer until then.
  onChunk.close();
}

parentPort!.postMessage({ done: true, error: failure });

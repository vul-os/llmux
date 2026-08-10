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
  /**
   * Two Int32s shared with the main thread:
   *   [0] stop  — non-zero means THIS stream's consumer asked to stop, via
   *               break/return/throw or its own AbortSignal (index.ts's
   *               cancelNative sets it before ever calling llmux_cancel).
   *               Also doubles as how this file tells "our own stream asked
   *               for this" from "some other call on the same gateway got
   *               cancelled instead" when llmux_stream comes back with
   *               "context canceled" — see the error handling below.
   *   [1] acks  — how many chunks the consumer has actually pulled
   * The second one is the backpressure channel; see the wait loop below.
   */
  control: SharedArrayBuffer;
}

/** How far the library may run ahead of the consumer, in chunks. */
const PREFETCH = 1;

const { libPath, handle, request, control } = workerData as StreamWorkerData;
const ctl = new Int32Array(control);
const STOP = 0;
const ACKS = 1;

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
let native = 0;
const onChunk = new JSCallback(
  (chunkPtr: Pointer): number => {
    // chunk_json is owned by the library and valid only for this call. new
    // CString copies it, so nothing here outlives the return.
    native++;
    parentPort!.postMessage({ chunk: new CString(chunkPtr).toString(), native });

    // BACKPRESSURE. Without this the library runs to completion regardless of
    // what the consumer does: postMessage is fire-and-forget, so a `break` after
    // 3 chunks of a 10-chunk answer still generated and metered all 10 (measured
    // — see README.md, "What cancelling actually stops"). Blocking here until the
    // consumer is at most PREFETCH chunks behind bounds the overrun at PREFETCH.
    // llmux_cancel (below) closes the rest of the gap: it interrupts the
    // network read directly, instead of waiting for a chunk that is already
    // in flight to reach this callback at all.
    //
    // Blocking is legal on this thread and only on this thread: Atomics.wait
    // throws on a main thread, and this callback is running inside
    // llmux_stream, which is already blocking the worker.
    while (Atomics.load(ctl, STOP) === 0 && Atomics.load(ctl, ACKS) < native - PREFETCH) {
      // A bounded wait, re-checking STOP each time, so a lost notify degrades
      // into a 50 ms poll instead of a hang.
      Atomics.wait(ctl, ACKS, Atomics.load(ctl, ACKS), 50);
    }
    return Atomics.load(ctl, STOP);
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
    const msg = p ? new CString(p).toString() : "llmux_stream failed";
    if (p) symbols.llmux_free(p);
    // "context canceled" is the one message llmux_cancel is documented to
    // produce, and llmux_cancel is the only thing that produces it
    // (ffi/include/llmux.h). STOP is this stream's OWN cancel flag — the main
    // thread sets it, in the same order every time, before it ever calls
    // llmux_cancel (see index.ts's cancelNative). So if STOP is up when this
    // returns, the main thread's own break/return/throw/AbortSignal asked for
    // this, and it is not a failure to report — same principle the header
    // already states for a callback returning non-zero.
    //
    // If STOP is NOT up, we did not ask for this stream to stop, so something
    // else on this GATEWAY did: llmux_cancel is per-handle, so an unrelated
    // gw.cancel() call, or another stream's AbortSignal, reaches every call in
    // flight on the handle, this one included. That is surfaced rather than
    // swallowed — hiding it would make an externally-forced stop look
    // identical to this stream finishing on its own.
    if (msg !== "context canceled") {
      failure = msg; // an ordinary failure, unrelated to cancellation
    } else if (Atomics.load(ctl, STOP) === 0) {
      failure = "llmux_cancel was called on this gateway while this stream was still running " +
        "(it aborts every call in flight on the handle, not just one — see README.md)";
    }
    // else: "context canceled" AND our own STOP flag is up — our own cancel
    // coming back to us. Leave `failure` null.
  }
} catch (e) {
  failure = e instanceof Error ? e.message : "llmux_stream threw a non-Error value";
} finally {
  // Close the callback only after llmux_stream has unwound; the library holds
  // the pointer until then.
  onChunk.close();
}

parentPort!.postMessage({ done: true, error: failure, native });

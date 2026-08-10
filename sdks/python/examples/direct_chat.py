#!/usr/bin/env python3
"""Direct mode: llmux in this interpreter, through the C ABI.

    python3 examples/direct_chat.py

No server, no port, no loopback socket — libllmux is loaded with ctypes and the
gateway lives in this process.

Where the library comes from: $LLMUX_LIBRARY, else the bundled llmux/lib/, else
dist/ffi/<goos>_<goarch>/ in an enclosing checkout. Build one with
`scripts/build-ffi.sh` from the repo root.

Where the config comes from: $LLMUX_CONFIG_JSON if set (an llmux configuration
document, the same JSON `llmux serve` reads from llmux.json), else built-in
defaults plus auto-detected providers from the environment. `run-demo.sh` sets
it to a fake upstream so this runs with no provider account and no network.
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import llmux  # noqa: E402
from llmux import Gateway, LLMuxError  # noqa: E402

MODEL = os.environ.get("LLMUX_DEMO_MODEL", "demo")
CONFIG = os.environ.get("LLMUX_CONFIG_JSON") or None

# ffi/fakeupstream (what CONFIG above routes at) answers instantly and counts
# nothing, which is fine for models/chat/stream above but useless for showing
# that a cancelled stream_iter() actually stopped the provider instead of just
# stopping delivery to this process. That needs a provider slow enough to
# interrupt mid-stream and honest enough to report what it wrote:
# sdks/fake-upstream.py, the harness shared with every other language's SDK
# for this exact measurement. run-demo.sh starts a second instance of it and
# exports these two variables; run this file on its own and the section below
# prints why it is skipping instead of guessing at an upstream to talk to.
CANCEL_CONFIG = os.environ.get("LLMUX_CANCEL_CONFIG_JSON")
CANCEL_UPSTREAM_URL = os.environ.get("LLMUX_CANCEL_UPSTREAM_URL")


def delta(chunk: dict) -> str:
    """The text a chat.completion.chunk carries, if any.

    A chunk can legitimately carry no choices at all (a usage-only or
    role-only frame), so this indexes defensively — the same care an SSE
    reader needs against the HTTP API, because it is the same JSON.
    """
    choices = chunk.get("choices") or []
    if not choices:
        return ""
    return choices[0].get("delta", {}).get("content") or ""


def main() -> int:
    # 1. Probe the library before committing to it. A stale libllmux earlier on
    #    the load path otherwise misbehaves in ways that look like llmux bugs.
    print(f"library:     {llmux.library_path()}")
    print(f"abi version: {llmux.abi_version()}")

    # 2. The context manager is the whole handle-safety story. __exit__ closes
    #    on every path, including the exception below.
    with Gateway(CONFIG) as gw:
        print(f"gateway:     {gw!r}\n")

        # --- unary: the model list, answered from memory ------------------
        models = gw.models()
        ids = [m["id"] for m in models.get("data", [])]
        print(f"models       {len(ids)} available: {', '.join(ids[:4])}"
              f"{' ...' if len(ids) > 4 else ''}")

        # --- unary: one chat completion -----------------------------------
        answer = gw.chat(
            {
                "model": MODEL,
                "messages": [{"role": "user", "content": "say something short"}],
            }
        )
        choice = answer["choices"][0]["message"]["content"]
        print(f"chat         {choice!r}")
        print(f"             routed to model {answer['model']!r}, "
              f"{answer['usage']['total_tokens']} tokens")

        # --- streaming, callback form: the shape of the ABI ---------------
        # The callback runs on THIS thread, synchronously, before stream()
        # returns. Proved rather than asserted:
        caller = threading.get_ident()
        threads: set[int] = set()
        words: list[str] = []

        def on_chunk(chunk: dict) -> None:
            threads.add(threading.get_ident())
            words.append(delta(chunk))

        gw.stream({"model": MODEL, "messages": [{"role": "user", "content": "go"}]}, on_chunk)
        print(f"stream       {len(words)} chunks: {''.join(words)!r}")
        print(f"             callback thread(s) {threads}, caller {caller} "
              f"-> {'same thread' if threads == {caller} else 'DIFFERENT THREAD'}")

        # --- streaming, generator form: the shape Python wants ------------
        # Costs a worker thread; see Gateway.stream_iter. `break` stops the
        # stream, which the ABI treats as a normal end, not a failure.
        got = []
        for chunk in gw.stream_iter(
            {"model": MODEL, "messages": [{"role": "user", "content": "go"}]}
        ):
            got.append(delta(chunk))
            if len(got) == 2:
                break
        print(f"stream_iter  stopped early after {len(got)} chunks: {''.join(got)!r}")

        # --- cancellation: abandoning stream_iter() must reach llmux_cancel -
        # The section above shows `break` stopping DELIVERY of chunks to this
        # process. That is not the same claim as stopping the PROVIDER from
        # generating them, and conflating the two was the actual bug: a
        # consumer that broke out of stream_iter() after 3 chunks saw the
        # provider run all remaining words to completion anyway, metered in
        # full, because the old implementation only set a local flag its
        # callback checked before forwarding the NEXT chunk — nothing told
        # the provider to stop. Proving the fix needs a provider slow enough
        # to interrupt mid-stream and honest enough to say what it wrote;
        # ffi/fakeupstream (what CONFIG above points at) is neither. See
        # CANCEL_CONFIG's definition above for where this one comes from.
        if CANCEL_CONFIG and CANCEL_UPSTREAM_URL:
            with Gateway(CANCEL_CONFIG) as cancel_gw:
                seen = 0
                for _ in cancel_gw.stream_iter(
                    {"model": "demo", "messages": [{"role": "user", "content": "go"}]}
                ):
                    seen += 1
                    if seen == 3:
                        break  # -> GeneratorExit at the yield -> finally: -> cancel()
                # cancel() has already returned by the time break above does,
                # but the upstream's own "has the client gone?" check runs on
                # its next scheduled write (up to 100ms out, see
                # sdks/fake-upstream.py's _peer_gone), not synchronously with
                # our cancel. Give it a moment before asking what it wrote.
                time.sleep(0.3)
                with urllib.request.urlopen(f"{CANCEL_UPSTREAM_URL}/generated") as r:
                    generated = json.load(r)["generated"]
            print(f"cancel       stream_iter stopped the consumer at {seen}; "
                  f"the upstream generated {generated} of 12")
        else:
            print("cancel       skipped: set LLMUX_CANCEL_CONFIG_JSON and "
                  "LLMUX_CANCEL_UPSTREAM_URL, or run via run-demo.sh which "
                  "starts the timed upstream this needs")

        # --- the error path -----------------------------------------------
        # Errors are plain UTF-8 text, not JSON. The library allocated that
        # string; the binding frees it with llmux_free before raising, so an
        # exception is not a leak.
        try:
            gw.call("no-such-method", {})
        except LLMuxError as exc:
            print(f"error        {exc}")

    # 3. Out of the with-block: closed, and use-after-close is a clean error
    #    rather than a segfault, because handles are registry keys not pointers.
    print(f"\nafter close: {gw!r}")
    try:
        gw.models()
    except LLMuxError as exc:
        print(f"use-after-close: {exc}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except LLMuxError as exc:
        print(f"llmux: {exc}", file=sys.stderr)
        sys.exit(1)

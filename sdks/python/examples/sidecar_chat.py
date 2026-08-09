#!/usr/bin/env python3
"""Sidecar mode: llmux as a child process on 127.0.0.1, spoken to over HTTP.

    python3 examples/sidecar_chat.py

You do not run a server. `llmux.start()` finds the binary ($LLMUX_BINARY, the
bundled llmux/bin/llmux, or `llmux` on PATH), picks a free loopback port,
launches it, polls /health, and registers an atexit hook that terminates it.
Start is lazy, singleton and thread-safe; calling it twice does not spawn twice.

This is the mode to reach for first in Python. It survives fork (the gateway is
not in your address space), it costs no 12 MB shared library, it works on every
platform the binary is built for, and per-tenant virtual keys and budgets are
enforced at the HTTP boundary — which direct mode deliberately does not do.

`run-demo.sh` points the sidecar at a fake upstream so this runs with no
provider account and no network.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import llmux  # noqa: E402
from llmux import LLMuxError  # noqa: E402

MODEL = os.environ.get("LLMUX_DEMO_MODEL", "demo")
API_KEY = os.environ.get("LLMUX_API_KEY", "llmux-local")


def delta(chunk: dict) -> str:
    """The text an SSE chat.completion.chunk carries, if any.

    Frames with no choices (usage-only, role-only) are normal; the direct-mode
    example indexes exactly as carefully, because it is the same JSON.
    """
    choices = chunk.get("choices") or []
    if not choices:
        return ""
    return choices[0].get("delta", {}).get("content") or ""


def post(path: str, body: dict, stream: bool = False):
    """One HTTP call against the sidecar, with urllib — no dependencies."""
    req = urllib.request.Request(
        llmux.openai_base_url() + path,
        data=json.dumps(body).encode(),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {API_KEY}",
        },
    )
    return urllib.request.urlopen(req, timeout=30)


def main() -> int:
    # try/finally, not just atexit: an exception below must not leave a gateway
    # process behind for the rest of this interpreter's life.
    base = llmux.start()
    try:
        print(f"sidecar:     {base}")
        print(f"openai url:  {llmux.openai_base_url()}\n")

        with urllib.request.urlopen(base + "/v1/models", timeout=10) as r:
            ids = [m["id"] for m in json.load(r).get("data", [])]
        print(f"models       {len(ids)} available: {', '.join(ids[:4])}"
              f"{' ...' if len(ids) > 4 else ''}")

        with post(
            "/chat/completions",
            {"model": MODEL, "messages": [{"role": "user", "content": "say something short"}]},
        ) as r:
            answer = json.load(r)
        print(f"chat         {answer['choices'][0]['message']['content']!r}")
        print(f"             routed to model {answer['model']!r}, "
              f"{answer['usage']['total_tokens']} tokens")

        # Streaming is server-sent events off a socket this process already
        # reads. No callback, no FFI, no GIL question — this is the sidecar's
        # structural advantage for streaming in any language.
        words: list[str] = []
        with post(
            "/chat/completions",
            {
                "model": MODEL,
                "messages": [{"role": "user", "content": "go"}],
                "stream": True,
            },
        ) as r:
            for raw in r:
                line = raw.decode().strip()
                if not line.startswith("data: "):
                    continue
                payload = line[len("data: "):]
                if payload == "[DONE]":
                    break
                words.append(delta(json.loads(payload)))
        print(f"stream       {len(words)} SSE frames: {''.join(words)!r}")

        # The same JSON contract means the same errors, over a different
        # transport: an HTTP status plus a body, rather than a char** err.
        try:
            post("/chat/completions", {"model": "no-such-model", "messages": []}).read()
        except urllib.error.HTTPError as exc:
            print(f"error        HTTP {exc.code}: {exc.read().decode()[:120]}")

        # The openai package, if installed, needs nothing but the base URL.
        try:
            import openai  # noqa: F401
        except ImportError:
            print("\n(the `openai` package is not installed; with it, "
                  "`llmux.OpenAI()` returns a client already pointed here)")
        else:
            client = llmux.OpenAI()
            r = client.chat.completions.create(
                model=MODEL, messages=[{"role": "user", "content": "hi"}]
            )
            print(f"openai sdk   {r.choices[0].message.content!r}")
    finally:
        llmux.stop()
        print("\nsidecar stopped")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except LLMuxError as exc:
        print(f"llmux: {exc}", file=sys.stderr)
        sys.exit(1)

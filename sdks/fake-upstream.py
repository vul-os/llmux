#!/usr/bin/env python3
"""An OpenAI-compatible fake upstream that COUNTS the chunks it generates.

`ffi/fakeupstream` is the fake every other example points llmux at, and it stays
that. This one exists for a question that one cannot answer: when a consumer
cancels a stream, how many chunks did the provider actually produce?

That number is the whole point of `llmux_cancel`. A cancellation that returns
promptly to the caller while the completion runs to the end upstream still costs
the full generation and still meters it, and from the consumer's side the two
are indistinguishable. So this upstream

  * sleeps `--chunk-delay-ms` before each chunk, so a stream is long enough to
    be cancelled in the middle of;
  * counts a chunk as generated only once it has been written to the socket; and
  * serves that count at `GET /generated`, after the fact, out of band.

It also stops generating the moment the client goes away — before each chunk it
peeks at the socket for a FIN. Without that check the count would keep climbing
into a dead connection and every language would report "10 of 10", which is the
bug this measurement is meant to detect, arrived at by accident.

Stdlib only, and the same three stdout lines `ffi/fakeupstream` prints, so a
runner that already parses one can parse the other:

    URL http://127.0.0.1:PORT
    CONFIG {"providers": [...], "routes": [...]}
    TEXT one two three four

The JavaScript runtimes have their own copy of this contract in each of
node/deno/bun's `examples/fake-upstream.mjs` — they spawn a .mjs so the suite
does not require a Python on the box to run a Node example. Both implementations
answer `GET /generated` with the same JSON.

Usage:
    python3 fake-upstream.py [--text "one two three four"]
                             [--chunk-delay-ms 0] [--addr 127.0.0.1:0]
"""

from __future__ import annotations

import argparse
import json
import select
import socket
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

_lock = threading.Lock()
_generated = 0  # chunks written to a socket, across all streams
_streams = 0  # streaming requests served
_disconnects = 0  # streams the client walked away from


def _bump(field: str, n: int = 1) -> None:
    global _generated, _streams, _disconnects
    with _lock:
        if field == "generated":
            _generated += n
        elif field == "streams":
            _streams += n
        else:
            _disconnects += n


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # Set from main(); class attributes so every connection thread sees them.
    text = "one two three four"
    chunk_delay_ms = 0

    def log_message(self, *args) -> None:  # keep the runner's output readable
        pass

    # -- helpers ------------------------------------------------------------

    def _read_json(self) -> dict:
        n = int(self.headers.get("content-length") or 0)
        if n <= 0:
            return {}
        try:
            return json.loads(self.rfile.read(n) or b"{}")
        except ValueError:
            return {}

    def _json(self, obj: dict) -> None:
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _peer_gone(self) -> bool:
        """True once the client has closed its end.

        A cancelled llmux call closes the upstream response body, which sends a
        FIN. Peeking for a zero-byte read is how we see it WITHOUT consuming
        anything, and it is what keeps the generated count honest: writing into
        a dead socket succeeds for a kilobyte or two of kernel buffer, so a
        writer that only watches for BrokenPipeError over-counts by however much
        the buffer holds.
        """
        try:
            ready, _, _ = select.select([self.connection], [], [], 0)
            if not ready:
                return False
            return self.connection.recv(1, socket.MSG_PEEK) == b""
        except OSError:
            return True

    # -- routes -------------------------------------------------------------

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler's spelling
        if self.path.startswith("/generated"):
            with _lock:
                self._json({"generated": _generated, "streams": _streams, "disconnects": _disconnects})
            return
        self.send_error(404)

    def do_POST(self) -> None:  # noqa: N802
        if self.path.endswith("/chat/completions"):
            self._chat(self._read_json())
        elif self.path.endswith("/embeddings"):
            self._json(
                {
                    "object": "list",
                    "model": "upstream-model",
                    "data": [{"object": "embedding", "index": 0, "embedding": [0.25, -0.5, 0.75]}],
                    "usage": {"prompt_tokens": 3, "total_tokens": 3},
                }
            )
        else:
            self.send_error(404)

    def _chat(self, req: dict) -> None:
        words = self.text.split()
        if not req.get("stream"):
            # A unary answer takes as long as the whole stream would, so a
            # caller measuring "did cancelling help?" is comparing like with
            # like.
            time.sleep(self.chunk_delay_ms / 1000.0 * len(words))
            self._json(
                {
                    "id": "chatcmpl-fake",
                    "object": "chat.completion",
                    "created": int(time.time()),
                    "model": "upstream-model",
                    "choices": [
                        {"index": 0, "message": {"role": "assistant", "content": self.text}, "finish_reason": "stop"}
                    ],
                    "usage": {"prompt_tokens": 7, "completion_tokens": len(words), "total_tokens": 7 + len(words)},
                }
            )
            return

        _bump("streams")
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("cache-control", "no-cache")
        self.send_header("connection", "close")  # SSE body is delimited by EOF
        self.end_headers()

        def frame(obj: dict) -> bool:
            """Write one SSE frame. False once the client has gone."""
            if self._peer_gone():
                return False
            try:
                self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")
                self.wfile.flush()
            except OSError:
                return False
            _bump("generated")
            return True

        for i, word in enumerate(words):
            if self.chunk_delay_ms:
                time.sleep(self.chunk_delay_ms / 1000.0)
            content = word if i == len(words) - 1 else word + " "
            ok = frame(
                {
                    "id": "chatcmpl-fake",
                    "object": "chat.completion.chunk",
                    "created": int(time.time()),
                    "model": "upstream-model",
                    "choices": [{"index": 0, "delta": {"content": content}, "finish_reason": None}],
                }
            )
            if not ok:
                _bump("disconnects")
                self.close_connection = True
                return

        frame(
            {
                "id": "chatcmpl-fake",
                "object": "chat.completion.chunk",
                "created": int(time.time()),
                "model": "upstream-model",
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            }
        )
        frame(
            {
                "id": "chatcmpl-fake",
                "object": "chat.completion.chunk",
                "created": int(time.time()),
                "model": "upstream-model",
                "choices": [],
                "usage": {"prompt_tokens": 7, "completion_tokens": len(words), "total_tokens": 7 + len(words)},
            }
        )
        try:
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except OSError:
            pass
        self.close_connection = True


def config_json(base_url: str) -> str:
    """An llmux configuration document routing the model "demo" at this server.

    Printing it here rather than composing it in each caller is the same reason
    `ffi/fakeupstream` prints one: otherwise every language carries its own copy
    of the same config literal and they drift apart silently.
    """
    return json.dumps(
        {
            "providers": [{"name": "fake", "type": "passthrough", "base_url": base_url.rstrip("/") + "/v1"}],
            "routes": [{"model": "demo", "provider": "fake", "target_model": "upstream-model"}],
        }
    )


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--text", default="one two three four", help="assistant text to answer with")
    ap.add_argument("--chunk-delay-ms", type=int, default=0, help="sleep before each streamed chunk")
    ap.add_argument("--addr", default="127.0.0.1:0", help="listen address")
    args = ap.parse_args()

    host, _, port = args.addr.rpartition(":")
    _Handler.text = args.text
    _Handler.chunk_delay_ms = args.chunk_delay_ms

    srv = ThreadingHTTPServer((host or "127.0.0.1", int(port)), _Handler)
    srv.daemon_threads = True
    base = f"http://{srv.server_address[0]}:{srv.server_address[1]}"

    print(f"URL {base}")
    print(f"CONFIG {config_json(base)}")
    print(f"TEXT {args.text}")
    sys.stdout.flush()  # the runner reads these line by line and would hang

    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()

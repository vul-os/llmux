"""A minimal OpenAI-compatible upstream, in-process, for the direct-mode tests.

The examples use `ffi/fakeupstream` — the same Go fake the C smoke test and the
latency benchmark drive. The tests cannot: `python3 -m unittest discover` must
work in a checkout with no Go toolchain and no build step. So this is a
stdlib-only stand-in, and its job is deliberately narrow — it exists to give the
ctypes BINDING a request path (a handle, a result to free, several stream
chunks, a callback to run), not to model llmux's behaviour. Anything about llmux
itself belongs in `ffi/`'s tests, against the Go fake.

It answers `/v1/chat/completions` (unary and SSE) and `/v1/embeddings`.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TEXT = "one two three four"


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # keep the test output clean
        pass

    def _read_json(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        try:
            return json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return {}

    def _send_json(self, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler's spelling
        req = self._read_json()
        if self.path.endswith("/embeddings"):
            self._send_json(
                {
                    "object": "list",
                    "model": req.get("model", ""),
                    "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}],
                    "usage": {"prompt_tokens": 3, "total_tokens": 3},
                }
            )
            return

        if not req.get("stream"):
            self._send_json(
                {
                    "id": "chatcmpl-test",
                    "object": "chat.completion",
                    "created": int(time.time()),
                    "model": req.get("model", ""),
                    "choices": [
                        {
                            "index": 0,
                            "message": {"role": "assistant", "content": TEXT},
                            "finish_reason": "stop",
                        }
                    ],
                    "usage": {"prompt_tokens": 7, "completion_tokens": 5, "total_tokens": 12},
                }
            )
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for i, word in enumerate(TEXT.split(" ")):
            frame = {
                "id": "chatcmpl-test",
                "object": "chat.completion.chunk",
                "created": int(time.time()),
                "model": req.get("model", ""),
                "choices": [
                    {"index": 0, "delta": {"content": word if i == 0 else " " + word}}
                ],
            }
            self.wfile.write(b"data: " + json.dumps(frame).encode() + b"\n\n")
            self.wfile.flush()
        self.wfile.write(
            b'data: {"id":"chatcmpl-test","object":"chat.completion.chunk","choices":'
            b'[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n'
        )
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True


class FakeUpstream:
    """Start on an ephemeral loopback port; ``config_json`` routes llmux at it."""

    def __init__(self) -> None:
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def config_json(self, model: str = "demo") -> str:
        return json.dumps(
            {
                "providers": [
                    {"name": "fake", "type": "passthrough", "base_url": self.base_url + "/v1"}
                ],
                "routes": [
                    {"model": model, "provider": "fake", "target_model": "upstream-model"}
                ],
            }
        )

    def stop(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

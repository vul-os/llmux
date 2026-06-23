#!/usr/bin/env python3
"""Test fixture: a fake llmux binary.

Honors LLMUX_ADDR=127.0.0.1:<port>, binds it, and serves GET /health -> 200.
Used so SDK tests do not need the real Go gateway. Behavior is controlled by env:

  FAKE_HEALTH_STATUS  HTTP status to return for /health (default 200)
  FAKE_NEVER_LISTEN   if "1", sleep without ever binding (simulates a hung start)
"""

import os
import signal
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer


def main() -> int:
    # Exit promptly on SIGTERM so the SDK's stop()/cleanup is fast and leaves
    # no orphans (some Python builds otherwise let serve_forever swallow it).
    signal.signal(signal.SIGTERM, lambda *_: os._exit(0))

    if os.environ.get("FAKE_NEVER_LISTEN") == "1":
        # Never serve health; the SDK's poll must time out.
        time.sleep(30)
        return 0

    addr = os.environ.get("LLMUX_ADDR", "127.0.0.1:0")
    host, _, port = addr.partition(":")
    status = int(os.environ.get("FAKE_HEALTH_STATUS", "200"))

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            if self.path == "/health":
                self.send_response(status)
                self.end_headers()
                self.wfile.write(b"ok")
            else:
                self.send_response(404)
                self.end_headers()

        def log_message(self, *args):  # silence
            pass

    srv = HTTPServer((host, int(port)), Handler)
    srv.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())

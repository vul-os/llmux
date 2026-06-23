#!/usr/bin/env python3
"""Fake llmux binary for .NET SDK tests.

Honors LLMUX_ADDR=127.0.0.1:<port>, serves GET /health -> 200.
  FAKE_HEALTH_STATUS  status for /health (default 200)
  FAKE_NEVER_LISTEN   if "1", never binds (simulates a hung start)
"""
import os
import signal
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer


def main():
    signal.signal(signal.SIGTERM, lambda *_: os._exit(0))
    if os.environ.get("FAKE_NEVER_LISTEN") == "1":
        time.sleep(30)
        return
    host, _, port = os.environ.get("LLMUX_ADDR", "127.0.0.1:0").partition(":")
    status = int(os.environ.get("FAKE_HEALTH_STATUS", "200"))

    class H(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/health":
                self.send_response(status)
                self.end_headers()
                self.wfile.write(b"ok")
            else:
                self.send_response(404)
                self.end_headers()

        def log_message(self, *a):
            pass

    HTTPServer((host, int(port)), H).serve_forever()


if __name__ == "__main__":
    sys.exit(main())

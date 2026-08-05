#!/usr/bin/env node
"use strict";
// Test fixture: a fake llmux binary (kept OUT of test/ so the test runner
// doesn't execute it as a test). Honors LLMUX_ADDR, serves GET /health -> 200.
//   FAKE_HEALTH_STATUS  status for /health (default 200)
//   FAKE_NEVER_LISTEN   if "1", never binds (simulates a hung start)
import * as http from "http";

process.on("SIGTERM", () => process.exit(0));
if (process.env.FAKE_NEVER_LISTEN === "1") {
  setTimeout(() => process.exit(0), 30000);
} else {
  const addr = process.env.LLMUX_ADDR || "127.0.0.1:0";
  const [host, portStr] = addr.split(":");
  // addr is always "host:port" in practice (see the default above and
  // index.ts's `LLMUX_ADDR: 127.0.0.1:${port}`), but a malformed override
  // would leave portStr undefined; fall back to an ephemeral port (0)
  // rather than passing NaN to listen().
  const status = parseInt(process.env.FAKE_HEALTH_STATUS || "200", 10);
  const srv = http.createServer((req, res) => {
    if (req.url === "/health") { res.writeHead(status); res.end("ok"); }
    else { res.writeHead(404); res.end(); }
  });
  srv.listen(parseInt(portStr ?? "0", 10), host);
}

"""Tests for the llmux Python sidecar launcher.

Run from sdks/python:  python3 -m unittest discover -s tests

Covers: binary resolution, URL formatting, health-poll readiness/timeout,
singleton/lazy start, and cleanup. An optional integration test drives the
real binary when LLMUX_BINARY (or the bundled bin) points at the Go gateway.
"""

from __future__ import annotations

import os
import socket
import stat
import sys
import tempfile
import textwrap
import time
import unittest
import unittest.mock
import urllib.request
from pathlib import Path

# Make the package importable when run from sdks/python.
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import llmux  # noqa: E402
from llmux import _sidecar  # noqa: E402

FAKE = Path(__file__).with_name("fake_llmux.py")


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _make_fake_binary(tmpdir: str, env: dict[str, str] | None = None) -> str:
    """Write an executable shell wrapper that runs the fake llmux fixture."""
    extra = env or {}
    exports = "\n".join(f'export {k}="{v}"' for k, v in extra.items())
    wrapper = Path(tmpdir) / "llmux"
    wrapper.write_text(
        textwrap.dedent(
            f"""\
            #!/bin/sh
            {exports}
            exec "{sys.executable}" "{FAKE}"
            """
        )
    )
    wrapper.chmod(wrapper.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return str(wrapper)


class SidecarTestBase(unittest.TestCase):
    def setUp(self):
        _reset_singleton()
        self._saved_env = dict(os.environ)

    def tearDown(self):
        _reset_singleton()
        os.environ.clear()
        os.environ.update(self._saved_env)


class BinaryResolutionTest(SidecarTestBase):
    def test_env_override_wins(self):
        with tempfile.TemporaryDirectory() as d:
            target = os.path.join(d, "custom-llmux")
            Path(target).write_text("#!/bin/sh\n")
            os.environ["LLMUX_BINARY"] = target
            self.assertEqual(_sidecar._binary_path(), target)

    def test_falls_back_to_path(self):
        # No LLMUX_BINARY, no bundled bin -> PATH lookup. Put a fake on PATH.
        with tempfile.TemporaryDirectory() as d:
            os.environ.pop("LLMUX_BINARY", None)
            tool = Path(d) / "llmux"
            tool.write_text("#!/bin/sh\n")
            tool.chmod(0o755)
            os.environ["PATH"] = d
            # Neutralize the bundled bin by pointing __file__'s bin elsewhere.
            with _no_bundled_bin():
                resolved = _sidecar._binary_path()
            self.assertEqual(Path(resolved).resolve(), tool.resolve())

    def test_clear_error_when_missing(self):
        os.environ.pop("LLMUX_BINARY", None)
        os.environ["PATH"] = ""  # nothing on PATH
        with _no_bundled_bin():
            with self.assertRaises(llmux.LLMuxError) as cm:
                _sidecar._binary_path()
        self.assertIn("llmux binary not found", str(cm.exception))


class UrlFormattingTest(SidecarTestBase):
    def test_openai_base_url_appends_v1(self):
        port = _free_port()
        _sidecar._base = f"http://127.0.0.1:{port}"

        class _Live:
            def poll(self):
                return None

        _sidecar._proc = _Live()
        self.assertEqual(_sidecar.base_url(), f"http://127.0.0.1:{port}")
        self.assertEqual(_sidecar.openai_base_url(), f"http://127.0.0.1:{port}/v1")
        self.assertTrue(_sidecar.openai_base_url().endswith("/v1"))


class HealthPollTest(SidecarTestBase):
    def test_becomes_ready_on_200(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["LLMUX_BINARY"] = _make_fake_binary(d)
            base = _sidecar.start(timeout=10.0)
            self.assertRegex(base, r"^http://127\.0\.0\.1:\d+$")
            with urllib.request.urlopen(base + "/health", timeout=2) as r:
                self.assertEqual(r.status, 200)

    def test_times_out_when_never_200(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["LLMUX_BINARY"] = _make_fake_binary(
                d, {"FAKE_HEALTH_STATUS": "503"}
            )
            with self.assertRaises(llmux.LLMuxError) as cm:
                _sidecar.start(timeout=0.6)
            self.assertIn("did not become healthy", str(cm.exception))
            # Failed start must not leave a process around.
            self.assertIsNone(_sidecar._proc)

    def test_times_out_when_unreachable(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["LLMUX_BINARY"] = _make_fake_binary(
                d, {"FAKE_NEVER_LISTEN": "1"}
            )
            with self.assertRaises(llmux.LLMuxError):
                _sidecar.start(timeout=0.6)

    def test_wait_healthy_helper_directly(self):
        # Stand up a real local server and exercise _wait_healthy in isolation.
        from http.server import BaseHTTPRequestHandler, HTTPServer
        import threading

        class H(BaseHTTPRequestHandler):
            def do_GET(self):  # noqa: N802
                self.send_response(200)
                self.end_headers()

            def log_message(self, *a):
                pass

        srv = HTTPServer(("127.0.0.1", 0), H)
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()
        try:
            base = f"http://127.0.0.1:{srv.server_address[1]}"
            _sidecar._wait_healthy(base, 3.0)  # should not raise
        finally:
            srv.shutdown()
            srv.server_close()


class SingletonTest(SidecarTestBase):
    def test_start_twice_same_base_no_respawn(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["LLMUX_BINARY"] = _make_fake_binary(d)
            b1 = _sidecar.start()
            proc1 = _sidecar._proc
            b2 = _sidecar.start()
            b3 = _sidecar.base_url()
            self.assertEqual(b1, b2)
            self.assertEqual(b1, b3)
            self.assertIs(_sidecar._proc, proc1, "must not respawn")


class CleanupTest(SidecarTestBase):
    def test_stop_kills_child_and_frees_port(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["LLMUX_BINARY"] = _make_fake_binary(d)
            base = _sidecar.start()
            port = int(base.rsplit(":", 1)[1])
            self.assertTrue(_port_open(port))
            _sidecar.stop()
            self.assertIsNone(_sidecar._proc)
            # Port should be freed shortly after the child dies.
            self.assertTrue(_wait_port_closed(port, 3.0), "port not freed after stop")


@unittest.skipUnless(
    _sidecar.os.environ.get("LLMUX_BINARY")
    or (Path(_sidecar.__file__).parent / "bin" / "llmux").exists(),
    "real llmux binary not available",
)
class IntegrationTest(SidecarTestBase):
    """Drives the SDK against the real Go binary when present.

    The base-class setUp scrubs the binary env per test, so we re-resolve here
    via the value captured at import time.
    """

    def test_end_to_end(self):
        # Honor an explicitly provided real binary; else rely on the bundled bin.
        real = self._saved_env.get("LLMUX_BINARY")
        if real:
            os.environ["LLMUX_BINARY"] = real
        base = _sidecar.start(timeout=15.0)
        self.assertRegex(base, r"^http://127\.0\.0\.1:\d+$")
        with urllib.request.urlopen(base + "/health", timeout=3) as r:
            self.assertEqual(r.status, 200)
        # openai base hands back a working /v1 root (models endpoint).
        self.assertTrue(_sidecar.openai_base_url().endswith("/v1"))


# --- helpers ---------------------------------------------------------------


def _reset_singleton():
    """Tear down any real child and clear the module singleton.

    Only calls the real stop() when _proc is an actual subprocess.Popen, so a
    test stub left in _proc can't break a subsequent test's teardown.
    """
    import subprocess

    proc = _sidecar._proc
    if isinstance(proc, subprocess.Popen):
        _sidecar.stop()
    _sidecar._proc = None
    _sidecar._base = None


class _no_bundled_bin:
    """Context manager: make the bundled bin/<name> path report non-existent.

    Patches Path.exists narrowly: only the bundled binary location returns
    False; every other path delegates to the original implementation.
    """

    def __enter__(self):
        bundled = (Path(_sidecar.__file__).parent / "bin" / "llmux").resolve()
        self._exists_orig = Path.exists

        def patched_exists(p):
            try:
                if p.resolve() == bundled:
                    return False
            except OSError:
                pass
            return self._exists_orig(p)

        self._patcher = unittest.mock.patch.object(Path, "exists", patched_exists)
        self._patcher.start()
        return self

    def __exit__(self, *exc):
        self._patcher.stop()
        return False


def _port_open(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        return s.connect_ex(("127.0.0.1", port)) == 0


def _wait_port_closed(port: int, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not _port_open(port):
            return True
        time.sleep(0.05)
    return not _port_open(port)


if __name__ == "__main__":
    unittest.main()

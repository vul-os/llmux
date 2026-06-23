"""Sidecar launcher: spawn the bundled llmux binary on a local port.

The local wedge: instead of running a server yourself, this starts the gateway
as a child process on 127.0.0.1 and hands you a base_url. Your existing OpenAI
client points at it unchanged — that's how llmux works in any language.
"""

from __future__ import annotations

import atexit
import os
import platform
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path


class LLMuxError(RuntimeError):
    pass


_lock = threading.Lock()
_proc: subprocess.Popen | None = None
_base: str | None = None


def _binary_path() -> str:
    # 1) explicit override
    env = os.environ.get("LLMUX_BINARY")
    if env:
        return env
    # 2) binary bundled in the package (shipped in platform wheels)
    name = "llmux.exe" if platform.system() == "Windows" else "llmux"
    bundled = Path(__file__).parent / "bin" / name
    if bundled.exists():
        return str(bundled)
    # 3) on PATH
    from shutil import which

    found = which("llmux")
    if found:
        return found
    raise LLMuxError(
        "llmux binary not found. Set LLMUX_BINARY, install a platform wheel, "
        "or build it: `go build -o sdks/python/llmux/bin/llmux ./cmd/llmux`"
    )


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_healthy(base: str, timeout: float) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(base + "/health", timeout=1) as r:
                if r.status == 200:
                    return
        except (urllib.error.URLError, ConnectionError, OSError) as e:
            last = e
        time.sleep(0.05)
    raise LLMuxError(f"llmux did not become healthy within {timeout}s: {last}")


def start(
    port: int | None = None,
    config: str | None = None,
    env: dict | None = None,
    timeout: float = 10.0,
) -> str:
    """Start the sidecar (idempotent) and return its base URL (http://host:port).

    Provider API keys are inherited from the environment (OPENAI_API_KEY, etc.),
    so the gateway auto-detects providers exactly like the standalone binary.
    """
    global _proc, _base
    with _lock:
        if _proc is not None and _proc.poll() is None:
            return _base  # already running

        port = port or _free_port()
        addr = f"127.0.0.1:{port}"
        child_env = dict(os.environ)
        child_env["LLMUX_ADDR"] = addr
        if config:
            child_env["LLMUX_CONFIG"] = config
        if env:
            child_env.update(env)

        cmd = [_binary_path()]
        _proc = subprocess.Popen(cmd, env=child_env)
        _base = f"http://{addr}"
        try:
            _wait_healthy(_base, timeout)
        except Exception:
            # _lock is already held here; use the unlocked variant to avoid a
            # self-deadlock on the non-reentrant lock.
            _stop_locked()
            raise
        atexit.register(stop)
        return _base


def base_url() -> str:
    """Return the running sidecar base URL, starting it if needed."""
    if _proc is None or _proc.poll() is not None:
        return start()
    return _base


def openai_base_url() -> str:
    """Return the OpenAI-style base URL (…/v1) for SDK base_url= arguments."""
    return base_url() + "/v1"


def stop() -> None:
    """Stop the sidecar if running."""
    with _lock:
        _stop_locked()


def _stop_locked() -> None:
    """Terminate the child and clear state. Caller must hold ``_lock``."""
    global _proc
    if _proc is not None and _proc.poll() is None:
        _proc.terminate()
        try:
            _proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            _proc.kill()
    _proc = None

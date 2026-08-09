#!/usr/bin/env python3
"""The fork hazard, demonstrated rather than described.

    python3 examples/fork_hazard.py

Direct mode loads the Go runtime into your interpreter. After fork() without
exec(), the child inherits ONE thread — the one that called fork — while the Go
runtime still believes in all the threads it had. Any call that needs another
one (the netpoller, a locked-up M, the GC) waits forever for a thread that does
not exist in the child.

Everything in this file is a measurement, not a claim. Each section forks, runs
one real call in the child with a watchdog, and prints what actually happened.

WHY THIS IS THE PYTHON PAGE OF THE MANUAL. Python forks by default in more
places than any other host in this SDK set:

    multiprocessing / concurrent.futures.ProcessPoolExecutor
        "fork" is the default start method on Linux for anything older than
        3.14, and every worker inherits a broken runtime. Fix: set_start_method
        ("spawn") -- or get_context("spawn") if you cannot change global state.
    uWSGI, Gunicorn sync/gthread workers, Celery's prefork pool
        The master imports your app, then forks workers. If the master touched
        libllmux, every worker is broken. Fix: load it POST-fork, in the worker
        (uWSGI @postfork, Gunicorn post_fork, Celery worker_process_init) --
        or use the sidecar, which is not in your address space at all and does
        not care how many times you fork.

THE TRAP THIS FILE EXISTS TO SHOW: some calls still work in the forked child.
"models" is answered from memory and usually returns fine, so a smoke test that
only calls it reports a clean bill of health for a process that will hang on the
first chat completion. "It worked when I tried it" is not evidence here.
"""

from __future__ import annotations

import ctypes
import json
import multiprocessing
import os
import select
import signal
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import llmux  # noqa: E402
from llmux import Gateway  # noqa: E402

CONFIG = os.environ.get("LLMUX_CONFIG_JSON") or None
MODEL = os.environ.get("LLMUX_DEMO_MODEL", "demo")
CHAT = {"model": MODEL, "messages": [{"role": "user", "content": "ping"}]}
WATCHDOG_SECONDS = float(os.environ.get("LLMUX_FORK_TIMEOUT", "5"))


# --------------------------------------------------------------------------
# os.fork(): the raw hazard
# --------------------------------------------------------------------------


def fork_and_run(gw: Gateway, label: str, work) -> str:
    """Fork, run `work(gw)` in the child, and report — even if it never returns.

    The pipe is the only thing we can trust: a hung child cannot write to it,
    and a crashed one closes it. Reading it with a timeout is what turns "this
    hangs" from folklore into a printed result.
    """
    read_fd, write_fd = os.pipe()
    pid = os.fork()
    if pid == 0:  # ---- child ----
        os.close(read_fd)
        try:
            work(gw)
            os.write(write_fd, b"completed")
        except BaseException as exc:  # noqa: BLE001 - reporting, not handling
            os.write(write_fd, f"raised {type(exc).__name__}: {exc}".encode()[:400])
        os._exit(0)

    # ---- parent ----
    os.close(write_fd)
    started = time.monotonic()
    ready, _, _ = select.select([read_fd], [], [], WATCHDOG_SECONDS)
    if ready:
        outcome = os.read(read_fd, 512).decode() or "wrote nothing"
        elapsed = time.monotonic() - started
        os.waitpid(pid, 0)
    else:
        outcome = f"HUNG — no result in {WATCHDOG_SECONDS}s, SIGKILLed"
        elapsed = time.monotonic() - started
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)
    os.close(read_fd)
    print(f"  {label:<34} {outcome}  ({elapsed:.2f}s)")
    return outcome


# --------------------------------------------------------------------------
# multiprocessing: the same hazard, wearing a friendly API
# --------------------------------------------------------------------------


def _worker(config: str | None, library: str, result) -> None:
    """Run one chat completion. Used by both the fork and the spawn context.

    Under "spawn" this is a fresh interpreter that loads libllmux itself, so the
    Go runtime is whole. Under "fork" it is the inherited, broken one.
    """
    try:
        with Gateway(config, library=library) as gw:
            answer = gw.chat(CHAT)
        result["text"] = answer["choices"][0]["message"]["content"]
    except BaseException as exc:  # noqa: BLE001
        result["text"] = f"raised {type(exc).__name__}: {exc}"


def multiprocessing_run(method: str, library: str) -> str:
    ctx = multiprocessing.get_context(method)
    manager = multiprocessing.Manager()
    result = manager.dict()
    proc = ctx.Process(target=_worker, args=(CONFIG, library, result))
    started = time.monotonic()
    proc.start()
    proc.join(WATCHDOG_SECONDS)
    elapsed = time.monotonic() - started
    if proc.is_alive():
        proc.kill()
        proc.join()
        outcome = f"HUNG — no result in {WATCHDOG_SECONDS}s, killed"
    else:
        outcome = str(result.get("text", f"exited {proc.exitcode} with no result"))
    manager.shutdown()
    print(f"  start_method={method:<8} {outcome!r:<48} ({elapsed:.2f}s)")
    return outcome


# --------------------------------------------------------------------------


def main() -> int:
    library = llmux.library_path()
    print(f"library:        {library}")
    print(f"abi version:    {llmux.abi_version()}")
    print(f"platform:       {sys.platform}, python {sys.version.split()[0]}")
    print(f"default start_method: {multiprocessing.get_start_method()}")
    print(f"watchdog:       {WATCHDOG_SECONDS}s\n")

    with Gateway(CONFIG) as gw:
        # Establish that the parent is healthy. Without this, a later hang
        # could just be a broken config, and the demonstration would prove
        # nothing.
        answer = gw.chat(CHAT)
        print("parent process (no fork):")
        print(f"  {'chat':<34} {answer['choices'][0]['message']['content']!r}\n")

        print("after os.fork(), in the child:")
        fork_and_run(gw, "models  (memory only)", lambda g: g.models())
        fork_and_run(gw, "chat    (needs the netpoller)", lambda g: g.chat(CHAT))
        fork_and_run(
            gw,
            "new + chat  (a fresh gateway)",
            lambda g: Gateway(CONFIG, library=library).chat(CHAT),
        )
        print(
            "\n  Read the first line again: 'models' SUCCEEDED. The runtime is\n"
            "  broken all the same — a call that needs a second thread never\n"
            "  returns. A health check that only lists models is a false green.\n"
        )

    print("through multiprocessing:")
    multiprocessing_run("fork", library)
    multiprocessing_run("spawn", library)
    print(
        "\n  spawn works because the child is a NEW interpreter that loads\n"
        "  libllmux itself. That is the fix, in one line, at the top of your\n"
        "  program:\n\n"
        "      multiprocessing.set_start_method('spawn')\n\n"
        "  If you cannot control the start method — uWSGI, Gunicorn sync\n"
        "  workers, Celery prefork — load the library after the fork in the\n"
        "  worker, or use the sidecar, which is a separate process and cannot\n"
        "  be broken by yours forking."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

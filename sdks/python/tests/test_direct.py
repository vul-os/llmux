"""Tests for direct mode — libllmux loaded into this interpreter with ctypes.

Run from sdks/python:  python3 -m unittest discover -s tests

The whole file skips when no shared library is resolvable, which is the honest
outcome on a platform nobody has built one for. Set LLMUX_LIBRARY to force a
particular one.

What is worth testing in a ctypes binding is not "does chat work" — ffi/ tests
that in Go and in C. It is the five things a binding gets wrong:

  1. Ownership. llmux_call returns malloc'd memory that only llmux_free may
     release; a c_char_p restype would silently leak every result.
  2. Handles on the error path. A gateway left open by an exception is open
     until the interpreter exits.
  3. The callback contract. Which thread it runs on, and what happens to an
     exception raised inside it.
  4. Abort. A callback that says stop must stop, and must not be an error.
  5. Cancellation. Abandoning stream_iter() (break, close(), garbage
     collection) or calling cancel() explicitly must reach llmux_cancel and
     actually stop the provider — not just stop delivering chunks locally,
     which is the bug the Cancellation tests below exist to catch. Proving it
     needs a provider slow enough to interrupt mid-stream and honest enough to
     report what it generated, which fake_openai's FakeUpstream is not built
     for (no delay, no counter) — the Cancellation tests use
     sdks/fake-upstream.py instead, the harness shared with every other
     language's SDK for this exact measurement.
"""

from __future__ import annotations

import ctypes
import json
import subprocess
import sys
import threading
import time
import unittest
import unittest.mock
import urllib.request
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from fake_openai import TEXT, FakeUpstream  # noqa: E402

import llmux  # noqa: E402
from llmux import Gateway, LLMuxError, LLMuxLibraryError  # noqa: E402
from llmux import _direct  # noqa: E402

CHAT = {"model": "demo", "messages": [{"role": "user", "content": "ping"}]}


def _library_or_skip() -> str:
    try:
        return llmux.library_path()
    except LLMuxLibraryError as exc:
        raise unittest.SkipTest(f"no libllmux on this machine: {exc}") from exc


class DirectTestBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.library = _library_or_skip()
        cls.upstream = FakeUpstream()
        cls.config = cls.upstream.config_json()

    @classmethod
    def tearDownClass(cls) -> None:
        if hasattr(cls, "upstream"):
            cls.upstream.stop()

    def gateway(self) -> Gateway:
        gw = Gateway(self.config, library=self.library)
        self.addCleanup(gw.close)
        return gw


class LibraryResolution(unittest.TestCase):
    def test_missing_library_names_where_it_looked(self):
        with unittest.mock.patch.dict("os.environ", {"LLMUX_LIBRARY": ""}, clear=False):
            with unittest.mock.patch.object(_direct, "_candidates", return_value=[]):
                with unittest.mock.patch.object(
                    _direct.ctypes, "CDLL", side_effect=OSError("nope")
                ):
                    with self.assertRaises(LLMuxLibraryError) as caught:
                        llmux.library_path()
        message = str(caught.exception)
        self.assertIn("LLMUX_LIBRARY", message)
        self.assertIn("Looked at:", message)

    def test_explicit_path_wins(self):
        self.assertEqual(llmux.library_path("/nowhere/libllmux.dylib"), "/nowhere/libllmux.dylib")


class Version(DirectTestBase):
    def test_abi_version_is_the_repo_version(self):
        version_file = Path(__file__).resolve().parents[3] / "VERSION"
        got = llmux.abi_version(llmux.load_library(self.library))
        if version_file.exists():
            self.assertEqual(got, version_file.read_text().strip())
        else:  # installed package, no checkout
            self.assertRegex(got, r"^\d+\.\d+\.\d+")

    def test_require_version_rejects_a_stale_library(self):
        with self.assertRaises(LLMuxLibraryError) as caught:
            llmux.load_library(self.library, require_version="0.0.0-not-a-real-version")
        self.assertIn("stale library", str(caught.exception))


class Lifecycle(DirectTestBase):
    def test_context_manager_closes(self):
        with Gateway(self.config, library=self.library) as gw:
            self.assertFalse(gw.closed)
        self.assertTrue(gw.closed)

    def test_context_manager_closes_on_the_error_path(self):
        """The reason the context manager exists at all."""
        gw = None
        with self.assertRaises(ZeroDivisionError):
            with Gateway(self.config, library=self.library) as gw:
                1 / 0
        self.assertTrue(gw.closed)

    def test_close_is_idempotent(self):
        gw = Gateway(self.config, library=self.library)
        gw.close()
        gw.close()
        self.assertTrue(gw.closed)

    def test_use_after_close_is_a_clean_error(self):
        gw = Gateway(self.config, library=self.library)
        gw.close()
        with self.assertRaises(LLMuxError):
            gw.models()

    def test_a_bad_config_raises_and_leaves_no_handle(self):
        with self.assertRaises(LLMuxError) as caught:
            Gateway("{not json", library=self.library)
        self.assertTrue(str(caught.exception))

    def test_two_gateways_coexist(self):
        with self.gateway() as a, self.gateway() as b:
            self.assertNotEqual(a.handle, b.handle)
            self.assertTrue(a.models())
            self.assertTrue(b.models())


class Calls(DirectTestBase):
    def test_models(self):
        with self.gateway() as gw:
            self.assertEqual(gw.models()["object"], "list")

    def test_chat(self):
        with self.gateway() as gw:
            answer = gw.chat(CHAT)
        self.assertEqual(answer["object"], "chat.completion")
        self.assertEqual(answer["choices"][0]["message"]["content"], TEXT)
        self.assertEqual(answer["model"], "upstream-model")  # routing ran

    def test_embed(self):
        with self.gateway() as gw:
            answer = gw.embed({"model": "demo", "input": "hello"})
        self.assertEqual(answer["data"][0]["object"], "embedding")

    def test_unknown_method_is_an_exception_not_a_crash(self):
        with self.gateway() as gw:
            with self.assertRaises(LLMuxError) as caught:
                gw.call("no-such-method", {})
        self.assertIn("unknown method", str(caught.exception))

    def test_streaming_via_call_is_refused(self):
        with self.gateway() as gw:
            with self.assertRaises(LLMuxError):
                gw.chat({**CHAT, "stream": True})

    def test_an_invented_handle_is_an_error_not_a_segfault(self):
        gw = self.gateway()
        gw._handle = 999999  # a handle that was never issued
        with self.assertRaises(LLMuxError) as caught:
            gw.models()
        self.assertIn("unknown handle", str(caught.exception))
        gw._closed = True  # do not close a handle we invented

    def test_results_are_freed_not_leaked(self):
        """The ownership rule, asserted rather than trusted.

        POINTER(c_char) + an explicit llmux_free is the only correct shape; a
        c_char_p restype loses the pointer and leaks. This counts the frees.
        """
        lib = llmux.load_library(self.library)
        freed: list[int] = []
        real_free = lib.llmux_free

        def counting_free(ptr):
            if ptr:
                freed.append(ctypes.cast(ptr, ctypes.c_void_p).value or 0)
            return real_free(ptr)

        with unittest.mock.patch.object(lib, "llmux_free", counting_free):
            gw = Gateway(self.config, library=lib)
            try:
                gw.models()
                gw.chat(CHAT)
                with self.assertRaises(LLMuxError):
                    gw.call("no-such-method", {})  # the error string is freed too
            finally:
                gw.close()

        # two results + one error message
        self.assertEqual(len(freed), 3, f"freed {len(freed)} allocations, expected 3")
        self.assertEqual(len(set(freed)), len(freed), "the same pointer was freed twice")


class Streaming(DirectTestBase):
    def test_chunks_reassemble_into_the_answer(self):
        seen: list[str] = []
        with self.gateway() as gw:
            gw.stream(CHAT, lambda chunk: seen.append(_delta(chunk)))
        self.assertGreaterEqual(len(seen), 4, "delivered as one blob, not streamed")
        self.assertEqual("".join(seen), TEXT)

    def test_the_callback_runs_on_the_calling_thread(self):
        """The claim in llmux.h, measured from Python.

        ffi/ctest/smoke.c compares pthread_self(); this compares the Python
        thread identity, which is what a ctypes binding actually cares about —
        it is what decides whether thread-local state, contextvars and the GIL
        behave the way the surrounding code assumes.
        """
        caller = threading.get_ident()
        threads: set[int] = set()
        with self.gateway() as gw:
            gw.stream(CHAT, lambda _chunk: threads.add(threading.get_ident()))
        self.assertEqual(threads, {caller})

    def test_the_callback_also_runs_on_a_worker_thread_that_calls(self):
        """Same claim, from a non-main thread, so 'the main thread' is not the
        real explanation for the result above."""
        threads: set[int] = set()
        worker_id: list[int] = []

        def run():
            worker_id.append(threading.get_ident())
            with self.gateway() as gw:
                gw.stream(CHAT, lambda _chunk: threads.add(threading.get_ident()))

        thread = threading.Thread(target=run)
        thread.start()
        thread.join(30)
        self.assertFalse(thread.is_alive())
        self.assertEqual(threads, {worker_id[0]})
        self.assertNotEqual(worker_id[0], threading.get_ident())

    def test_returning_false_stops_the_stream_and_is_not_an_error(self):
        seen: list[str] = []

        def stop_after_two(chunk):
            seen.append(_delta(chunk))
            return False if len(seen) == 2 else None

        with self.gateway() as gw:
            gw.stream(CHAT, stop_after_two)  # must not raise
        self.assertEqual(len(seen), 2)

    def test_an_exception_in_the_callback_propagates(self):
        """Without the trampoline's re-raise, ctypes prints the traceback and
        returns 0 to Go: the error is swallowed and the stream carries on."""
        seen: list[int] = []

        def explode(_chunk):
            seen.append(1)
            raise RuntimeError("callback said no")

        with self.gateway() as gw:
            with self.assertRaises(RuntimeError) as caught:
                gw.stream(CHAT, explode)
        self.assertEqual(str(caught.exception), "callback said no")
        self.assertEqual(len(seen), 1, "the stream kept going after the exception")

    def test_raw_gives_the_undecoded_chunk(self):
        seen: list[str] = []
        with self.gateway() as gw:
            gw.stream(CHAT, seen.append, raw=True)
        self.assertTrue(all(isinstance(s, str) for s in seen))
        self.assertEqual(json.loads(seen[0])["object"], "chat.completion.chunk")

    def test_stream_iter_yields_the_same_chunks(self):
        with self.gateway() as gw:
            text = "".join(_delta(chunk) for chunk in gw.stream_iter(CHAT))
        self.assertEqual(text, TEXT)

    def test_stream_iter_break_stops_the_worker(self):
        before = threading.active_count()
        with self.gateway() as gw:
            for _ in gw.stream_iter(CHAT):
                break
        for _ in range(50):
            if threading.active_count() <= before:
                break
            threading.Event().wait(0.05)
        self.assertLessEqual(threading.active_count(), before, "the stream worker outlived the loop")

    def test_stream_iter_propagates_errors(self):
        with self.gateway() as gw:
            with self.assertRaises(LLMuxError):
                list(gw.stream_iter({"model": "no-such-model", "messages": []}))


class Cancellation(unittest.TestCase):
    """llmux_cancel, and the bug it exists to fix.

    fake_openai.FakeUpstream (everything above this class) answers instantly
    and counts nothing — its only job is giving the ctypes BINDING a request
    path to exercise. Whether a cancellation actually reaches the provider is
    a question about the whole stack, not the binding, and answering it needs
    a provider slow enough to interrupt mid-stream and honest enough to say
    what it wrote: sdks/fake-upstream.py, the harness shared with every other
    language's SDK for exactly this measurement. It serves ten words at
    ``--chunk-delay-ms 100`` and counts chunks at ``GET /generated`` — see its
    module docstring for why that count only rises for chunks actually
    written to a socket, and stops the instant the client disconnects.

    Also stdlib-only, also skipped (via _library_or_skip) when no libllmux is
    resolvable on this machine, same as every other class here.
    """

    TEXT = "one two three four five six seven eight nine ten"
    CHAT = {"model": "demo", "messages": [{"role": "user", "content": "go"}]}

    @classmethod
    def setUpClass(cls) -> None:
        cls.library = _library_or_skip()
        harness = Path(__file__).resolve().parents[2] / "fake-upstream.py"
        cls.proc = subprocess.Popen(
            [sys.executable, str(harness), "--chunk-delay-ms", "100", "--text", cls.TEXT],
            stdout=subprocess.PIPE,
            text=True,
        )
        cls.url = None
        cls.config = None
        # The harness prints exactly three lines (URL, CONFIG, TEXT) and then
        # starts serving — see its module docstring. Stop reading at TEXT
        # rather than looping on readline() forever if it never appears.
        for _ in range(200):
            line = cls.proc.stdout.readline()
            if not line:
                break
            if line.startswith("URL "):
                cls.url = line[len("URL "):].strip()
            elif line.startswith("CONFIG "):
                cls.config = line[len("CONFIG "):].strip()
            elif line.startswith("TEXT "):
                break
        if not cls.url or not cls.config:
            raise unittest.SkipTest("sdks/fake-upstream.py did not start")

    @classmethod
    def tearDownClass(cls) -> None:
        if hasattr(cls, "proc"):
            cls.proc.terminate()
            try:
                cls.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:  # pragma: no cover - defensive
                cls.proc.kill()

    def gateway(self) -> Gateway:
        gw = Gateway(self.config, library=self.library)
        self.addCleanup(gw.close)
        return gw

    def _generated(self) -> int:
        with urllib.request.urlopen(f"{self.url}/generated") as resp:
            return json.load(resp)["generated"]

    def test_cancel_from_another_thread_unblocks_a_blocked_stream(self):
        """Fact measured with the coordinator's C probe, reproduced here:
        cancelling from another thread while `stream` blocks returns the
        blocked call with `context canceled` well before the 12-chunk stream
        (10 words + a finish frame + a usage frame) would end on its own —
        ~1.2s of chunk delay against an assertion of well under a second.

        This is possible only because a ctypes.CDLL call — llmux_stream is
        bound via CDLL, not PyDLL — releases the GIL for the duration of the
        native call. A PYFUNCTYPE/PyDLL binding would hold the GIL through
        the block and this second thread could never even reach the call to
        cancel(), let alone have it take effect.
        """
        gw = self.gateway()
        seen: list[Any] = []
        raised: list[BaseException] = []

        def blocked() -> None:
            try:
                gw.stream(self.CHAT, seen.append)
            except BaseException as exc:  # noqa: BLE001 - inspected below
                raised.append(exc)

        worker = threading.Thread(target=blocked)
        t0 = time.monotonic()
        worker.start()
        deadline = time.monotonic() + 5.0
        while len(seen) < 3 and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertGreaterEqual(len(seen), 3, "the upstream never delivered 3 chunks to cancel against")
        gw.cancel()
        worker.join(timeout=5)
        elapsed = time.monotonic() - t0

        self.assertFalse(worker.is_alive(), "cancel() did not unblock the blocked stream")
        self.assertEqual(len(raised), 1, "stream() must raise, not return quietly, on a cancel")
        self.assertIsInstance(raised[0], LLMuxError)
        self.assertIn("context canceled", str(raised[0]))
        self.assertLess(elapsed, 0.8, "cancel() took as long as letting the stream run to completion")

    def test_cancel_is_safe_from_inside_the_chunk_callback(self):
        """Fact measured with the coordinator's C probe, reproduced here:
        cancelling from inside the callback that is currently running does
        not deadlock, unlike close() (which must never be called from a
        callback because it waits on the very call running the callback).
        This is the only cancellation path open to a single-threaded host."""
        gw = self.gateway()
        seen: list[Any] = []

        def on_chunk(chunk: Any) -> None:
            seen.append(chunk)
            if len(seen) == 3:
                gw.cancel()

        with self.assertRaises(LLMuxError) as caught:
            gw.stream(self.CHAT, on_chunk)
        self.assertIn("context canceled", str(caught.exception))
        self.assertEqual(len(seen), 3)

    def test_handle_survives_a_cancel(self):
        """Fact measured with the coordinator's C probe: the handle stays
        usable immediately after a cancel, and a fresh call on it succeeds."""
        gw = self.gateway()
        gw.cancel()  # nothing running: a documented no-op, not an error
        self.assertTrue(gw.models())

    def test_stream_iter_break_stops_the_upstream_not_just_the_consumer(self):
        """The bug this task exists to fix.

        The previous stream_iter() only set a local flag that its callback
        checked before forwarding the NEXT chunk to Python — that stops
        chunks from reaching the consumer, but does nothing to the
        connection, so the provider kept generating (and llmux kept
        metering) chunks the consumer had already walked away from. This
        polls /generated because the provider's own disconnect detection
        happens on its next scheduled write, not the instant cancel() is
        called — see sdks/fake-upstream.py's _peer_gone for why that check
        cannot be synchronous with the client's disconnect.

        The upstream process (and so its /generated counter) is shared across
        every test in this class, hence the baseline: what matters is how
        much THIS stream added, not the running total.
        """
        baseline = self._generated()
        gw = self.gateway()
        count = 0
        for _ in gw.stream_iter(self.CHAT):
            count += 1
            if count == 3:
                break
        self.assertEqual(count, 3, "the consumer did not see 3 chunks before breaking")

        deadline = time.monotonic() + 2.0
        added = self._generated() - baseline
        while added > 3 and time.monotonic() < deadline:
            time.sleep(0.05)
            added = self._generated() - baseline
        self.assertEqual(
            added,
            3,
            f"stream_iter's break did not reach llmux_cancel: the upstream generated {added} "
            "chunks for this stream, not 3 -- it kept running after the consumer walked away",
        )

    def test_stream_iter_full_run_generates_all_twelve(self):
        """The baseline the cancellation number above is measured against:
        ten words plus a finish frame plus a usage frame, uninterrupted."""
        baseline = self._generated()
        gw = self.gateway()
        chunks = list(gw.stream_iter(self.CHAT))
        self.assertEqual("".join(_delta(c) for c in chunks), self.TEXT)
        self.assertEqual(self._generated() - baseline, 12)


def _delta(chunk: dict) -> str:
    choices = chunk.get("choices") or []
    if not choices:
        return ""
    return choices[0].get("delta", {}).get("content") or ""


if __name__ == "__main__":
    unittest.main()

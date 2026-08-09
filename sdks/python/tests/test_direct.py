"""Tests for direct mode — libllmux loaded into this interpreter with ctypes.

Run from sdks/python:  python3 -m unittest discover -s tests

The whole file skips when no shared library is resolvable, which is the honest
outcome on a platform nobody has built one for. Set LLMUX_LIBRARY to force a
particular one.

What is worth testing in a ctypes binding is not "does chat work" — ffi/ tests
that in Go and in C. It is the four things a binding gets wrong:

  1. Ownership. llmux_call returns malloc'd memory that only llmux_free may
     release; a c_char_p restype would silently leak every result.
  2. Handles on the error path. A gateway left open by an exception is open
     until the interpreter exits.
  3. The callback contract. Which thread it runs on, and what happens to an
     exception raised inside it.
  4. Abort. A callback that says stop must stop, and must not be an error.
"""

from __future__ import annotations

import ctypes
import json
import sys
import threading
import unittest
import unittest.mock
from pathlib import Path

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


def _delta(chunk: dict) -> str:
    choices = chunk.get("choices") or []
    if not choices:
        return ""
    return choices[0].get("delta", {}).get("content") or ""


if __name__ == "__main__":
    unittest.main()

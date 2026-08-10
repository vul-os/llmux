"""Direct mode: llmux running IN THIS PROCESS, through the C ABI.

The other half of this package is :mod:`llmux._sidecar`, which spawns the
``llmux`` binary and talks HTTP to it. This half loads ``libllmux`` with
:mod:`ctypes` and calls it in-process — no second process, no port, no loopback
socket. The JSON is identical either way: it is the JSON the HTTP API uses.

    from llmux import Gateway

    with Gateway() as gw:
        print(gw.models())
        print(gw.chat({"model": "demo", "messages": [...]}))

``ctypes`` is the whole dependency list. This module imports nothing outside the
standard library, deliberately: a binding that needs ``cffi`` compiled against
your interpreter is a worse deal than the sidecar for most people.

READ ``sdks/python/README.md`` BEFORE CHOOSING THIS. It loads the Go runtime
into your interpreter, and it is not fork-safe — which in Python means
``multiprocessing`` with the ``fork`` start method, uWSGI, and Gunicorn's sync
workers. ``examples/fork_hazard.py`` demonstrates the failure rather than
describing it.
"""

from __future__ import annotations

import ctypes
import json
import os
import platform
import queue
import sys
import threading
from pathlib import Path
from typing import Any, Callable, Iterator, Mapping, Optional, Sequence

from ._sidecar import LLMuxError

__all__ = [
    "Gateway",
    "LLMuxLibraryError",
    "abi_version",
    "library_path",
    "load_library",
]


class LLMuxLibraryError(LLMuxError):
    """The shared library could not be found, loaded, or is the wrong build."""


# --------------------------------------------------------------------------
# Finding the library
# --------------------------------------------------------------------------


def _lib_filename() -> str:
    system = platform.system()
    if system == "Darwin":
        return "libllmux.dylib"
    if system == "Windows":
        return "llmux.dll"
    return "libllmux.so"


def _go_platform() -> tuple[str, str]:
    """(GOOS, GOARCH) for this interpreter, to match dist/ffi's layout."""
    goos = {"Darwin": "darwin", "Linux": "linux", "Windows": "windows"}.get(
        platform.system(), platform.system().lower()
    )
    machine = platform.machine().lower()
    goarch = {
        "arm64": "arm64",
        "aarch64": "arm64",
        "x86_64": "amd64",
        "amd64": "amd64",
    }.get(machine, machine)
    return goos, goarch


def _candidates() -> list[Path]:
    """Where to look, in order. Most specific first."""
    name = _lib_filename()
    goos, goarch = _go_platform()
    out: list[Path] = []

    # 1. Bundled in the wheel, the way the sidecar binary is bundled.
    out.append(Path(__file__).resolve().parent / "lib" / name)

    # 2. A checkout: scripts/build-ffi.sh writes dist/ffi/<goos>_<goarch>/.
    #    Walk up from this file so `python examples/direct_chat.py` works in a
    #    clone with no install step.
    for parent in Path(__file__).resolve().parents:
        out.append(parent / "dist" / "ffi" / f"{goos}_{goarch}" / name)
        if (parent / ".git").exists():
            break

    return out


def library_path(path: str | os.PathLike[str] | None = None) -> str:
    """Resolve the shared library path, or raise a message that says where it looked.

    Order: the ``path`` argument, then ``$LLMUX_LIBRARY``, then the bundled
    ``llmux/lib/``, then ``dist/ffi/<goos>_<goarch>/`` in an enclosing checkout,
    then the platform loader's own search path.
    """
    if path:
        return str(path)
    env = os.environ.get("LLMUX_LIBRARY")
    if env:
        return env

    tried: list[str] = []
    for candidate in _candidates():
        if candidate.exists():
            return str(candidate)
        tried.append(str(candidate))

    # Last resort: let the platform loader search DYLD/LD_LIBRARY_PATH etc.
    name = _lib_filename()
    try:
        ctypes.CDLL(name)
        return name
    except OSError:
        tried.append(f"{name} (via the platform loader's search path)")

    goos, goarch = _go_platform()
    note = ""
    if (goos, goarch) not in {("darwin", "arm64"), ("linux", "arm64"), ("linux", "amd64")}:
        note = (
            f"\n\nNo llmux shared library has ever been built for {goos}/{goarch}. "
            "Prebuilt artifacts exist for darwin/arm64 and linux/arm64; linux/amd64 is "
            "built in CI. windows/amd64 and darwin/amd64 do not exist. Use the sidecar "
            "(`import llmux; llmux.start()`) — it is a supported answer, not a fallback."
        )
    raise LLMuxLibraryError(
        "libllmux not found. Set LLMUX_LIBRARY=/path/to/"
        + name
        + ", or build it with `scripts/build-ffi.sh` from an llmux checkout.\nLooked at:\n  "
        + "\n  ".join(tried)
        + note
    )


# --------------------------------------------------------------------------
# Binding
# --------------------------------------------------------------------------

# llmux_call and llmux_stream hand back malloc'd memory that only llmux_free may
# release. ctypes' c_char_p restype would copy the bytes and DISCARD THE
# POINTER, leaking every result — so results come back as POINTER(c_char) and
# _take() copies then frees. This is the single most important detail in the
# file.
_CharPtr = ctypes.POINTER(ctypes.c_char)
_ErrPtr = ctypes.POINTER(ctypes.c_char_p)

#: The chunk callback. CFUNCTYPE (not PYFUNCTYPE) reacquires the GIL for us.
ChunkCallback = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p)

_loaded: dict[str, ctypes.CDLL] = {}
_loaded_lock = threading.Lock()


def load_library(
    path: str | os.PathLike[str] | None = None, require_version: str | None = None
) -> ctypes.CDLL:
    """Load (once per path) and bind ``libllmux``.

    ``require_version`` compares :func:`abi_version` against what your bindings
    were written for. Worth doing at startup: a shared library is resolved off a
    load path you may not control, and a stale ``libllmux`` earlier on it
    otherwise misbehaves in ways that look like llmux bugs.
    """
    resolved = library_path(path)
    with _loaded_lock:
        lib = _loaded.get(resolved)
        if lib is None:
            try:
                lib = ctypes.CDLL(resolved)
            except OSError as exc:  # pragma: no cover - platform specific
                raise LLMuxLibraryError(f"could not load {resolved}: {exc}") from exc
            _bind(lib)
            _loaded[resolved] = lib

    if require_version is not None:
        got = abi_version(lib)
        if got != require_version:
            raise LLMuxLibraryError(
                f"{resolved} reports llmux {got}, these bindings want {require_version} "
                "— a stale library is on the load path"
            )
    return lib


def _bind(lib: ctypes.CDLL) -> None:
    lib.llmux_abi_version.restype = ctypes.c_char_p  # static; never freed
    lib.llmux_abi_version.argtypes = []

    lib.llmux_new.restype = ctypes.c_uint64
    lib.llmux_new.argtypes = [ctypes.c_char_p, _ErrPtr]

    lib.llmux_close.restype = None
    lib.llmux_close.argtypes = [ctypes.c_uint64]

    lib.llmux_cancel.restype = None
    lib.llmux_cancel.argtypes = [ctypes.c_uint64]

    lib.llmux_call.restype = _CharPtr
    lib.llmux_call.argtypes = [ctypes.c_uint64, ctypes.c_char_p, ctypes.c_char_p, _ErrPtr]

    lib.llmux_free.restype = None
    lib.llmux_free.argtypes = [_CharPtr]

    lib.llmux_stream.restype = ctypes.c_int
    lib.llmux_stream.argtypes = [
        ctypes.c_uint64,
        ctypes.c_char_p,
        ctypes.c_char_p,
        ChunkCallback,
        ctypes.c_void_p,
        _ErrPtr,
    ]


def abi_version(lib: ctypes.CDLL | None = None) -> str:
    """The llmux version the loaded library was built from."""
    lib = lib or load_library()
    return lib.llmux_abi_version().decode("utf-8")


def _take(lib: ctypes.CDLL, ptr: Any) -> str | None:
    """Copy a library-owned C string into Python and free the original."""
    if not ptr:
        return None
    try:
        return ctypes.cast(ptr, ctypes.c_char_p).value.decode("utf-8")  # type: ignore[union-attr]
    finally:
        lib.llmux_free(ptr)


def _take_err(lib: ctypes.CDLL, err: ctypes.c_char_p) -> str | None:
    """Same, for the trailing char** err out-parameter."""
    if not err.value:
        return None
    message = err.value.decode("utf-8", "replace")
    lib.llmux_free(ctypes.cast(err, _CharPtr))
    err.value = None
    return message


def _encode(body: Any) -> bytes | None:
    if body is None:
        return None
    if isinstance(body, bytes):
        return body
    if isinstance(body, str):
        return body.encode("utf-8")
    return json.dumps(body).encode("utf-8")


# --------------------------------------------------------------------------
# The handle
# --------------------------------------------------------------------------


class Gateway:
    """An llmux gateway running in this process.

    Use it as a context manager. ``__exit__`` closes the handle on every path,
    including an exception, which is the point: a handle leaked from an error
    path is a gateway that is never released for the life of the interpreter.

        with Gateway(config) as gw:
            gw.chat({...})

    Construction is inert. It starts no goroutines and — unless your config
    names a Postgres DSN — opens no sockets. Nothing happens until you call.

    A handle is safe to share between threads; so is this object.
    """

    def __init__(
        self,
        config: Mapping[str, Any] | str | bytes | None = None,
        *,
        library: str | os.PathLike[str] | ctypes.CDLL | None = None,
        require_version: str | None = None,
    ) -> None:
        self._lib = (
            library
            if isinstance(library, ctypes.CDLL)
            else load_library(library, require_version=require_version)
        )
        self._closed = False

        err = ctypes.c_char_p()
        handle = self._lib.llmux_new(_encode(config), ctypes.byref(err))
        message = _take_err(self._lib, err)
        if handle == 0:
            raise LLMuxError(message or "llmux_new failed without a message")
        self._handle = int(handle)

    # -- lifecycle ---------------------------------------------------------

    @property
    def handle(self) -> int:
        """The raw uint64 registry key, for code that calls the ABI directly."""
        return self._handle

    @property
    def closed(self) -> bool:
        return self._closed

    def close(self) -> None:
        """Release the gateway, aborting any stream still running on it.

        Idempotent, like ``llmux_close`` itself, so cleanup paths can be dumb.
        """
        if not self._closed:
            self._closed = True
            self._lib.llmux_close(ctypes.c_uint64(self._handle))

    def cancel(self) -> None:
        """Abort every call in flight on this handle, without closing it.

        The handle stays open and the next call on it starts on a fresh
        context — this is the ABI's answer to "I lost interest in the call
        that is running," not a substitute for :meth:`close`. A stream
        cancelled this way does not return quietly: the blocked ``call`` or
        ``stream`` raises :class:`LLMuxError` with the message
        ``context canceled``, because tokens already served are still
        metered and the caller should not mistake this for a normal end the
        way a callback returning ``False`` is.

        SAFE FROM ANOTHER THREAD while :meth:`call`, :meth:`stream`, or
        :meth:`stream_iter` blocks on this handle. This is not incidental:
        every ``ctypes.CDLL`` function releases the GIL for the duration of
        the native call, which is what lets a second Python thread run at
        all while the first is blocked inside ``llmux_stream`` — a
        ``ctypes.PyDLL`` binding would not have this property, since it
        assumes the GIL is already held and never lets go of it. Measured in
        ``tests/test_direct.py``
        (``test_cancel_from_another_thread_unblocks_a_blocked_stream``): the
        blocked call returns in well under the time the full stream would
        have taken.

        Also SAFE FROM INSIDE A CHUNK CALLBACK, unlike :meth:`close`, which
        must never be called from a callback because it waits (up to a few
        seconds) for the very call that is running the callback — that would
        be waiting on itself. A single-threaded host (a synchronous web
        framework, a callback-only script) has nowhere else to call this
        from, which is the reason it exists as a primitive separate from
        ``close`` rather than a flag ``close`` checks.

        A no-op if nothing is running on this handle, if the handle is
        unknown to the library, or if it has already been closed — so it is
        always safe to call, including from a ``finally`` block that does
        not know whether anything is in flight.

        PER-HANDLE, NOT PER-CALL: it aborts every call running on this
        gateway, not only the one you were thinking of. Two streams sharing
        one ``Gateway`` and cancelling one aborts both. If you need to cancel
        streams independently, give each its own ``Gateway`` — construction
        is inert (see the class docstring), so this costs you a handle, not
        a connection.
        """
        self._lib.llmux_cancel(ctypes.c_uint64(self._handle))

    def __enter__(self) -> "Gateway":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def __del__(self) -> None:  # best effort; the context manager is the contract
        try:
            self.close()
        except Exception:  # pragma: no cover - interpreter teardown
            pass

    def __repr__(self) -> str:
        state = "closed" if self._closed else "open"
        return f"<llmux.Gateway handle={self._handle} {state}>"

    # -- calls -------------------------------------------------------------

    def call(self, method: str, request: Any = None) -> Any:
        """One unary call. ``request`` is anything json-serializable, or None.

        Returns the parsed JSON response. Raises :class:`LLMuxError` with the
        library's message on failure — the message is plain text, not JSON, so
        it is not parsed.
        """
        if self._closed:
            raise LLMuxError(f"gateway {self._handle} is closed")
        err = ctypes.c_char_p()
        ptr = self._lib.llmux_call(
            ctypes.c_uint64(self._handle),
            method.encode("utf-8"),
            _encode(request),
            ctypes.byref(err),
        )
        result = _take(self._lib, ptr)
        message = _take_err(self._lib, err)
        if result is None:
            raise LLMuxError(message or f"llmux_call {method} failed without a message")
        return json.loads(result)

    def chat(self, request: Mapping[str, Any]) -> Any:
        """A chat completion. Same body as ``POST /v1/chat/completions``.

        ``"stream": true`` is refused here rather than quietly batched; use
        :meth:`stream` or :meth:`stream_iter`.
        """
        return self.call("chat", request)

    def embed(self, request: Mapping[str, Any]) -> Any:
        """An embeddings response. Same body as ``POST /v1/embeddings``."""
        return self.call("embed", request)

    def models(self) -> Any:
        """The OpenAI-shaped model list."""
        return self.call("models", None)

    # -- streaming ---------------------------------------------------------

    def stream(
        self,
        request: Mapping[str, Any],
        on_chunk: Callable[[Any], Any],
        *,
        raw: bool = False,
    ) -> None:
        """Stream a chat completion, calling ``on_chunk`` once per chunk.

        Blocks until the stream ends. ``on_chunk`` receives the parsed
        ``chat.completion.chunk`` (or the raw JSON string when ``raw=True``) and
        returns ``False`` to stop early — which is not an error, here or in the
        ABI. Any other return value, including ``None``, keeps streaming.

        The callback runs on THIS thread, synchronously, before ``stream``
        returns. That is measured, not assumed: ``tests/test_direct.py`` compares
        ``threading.get_ident()`` inside the callback against the caller's, and
        ``ffi/ctest/smoke.c`` does the same with ``pthread_self()``.

        An exception raised by ``on_chunk`` stops the stream and is re-raised
        here. Without that, ctypes would print the traceback to stderr and
        return 0 to Go — i.e. your error would be swallowed and the stream would
        carry on.
        """
        if self._closed:
            raise LLMuxError(f"gateway {self._handle} is closed")

        raised: list[BaseException] = []

        def trampoline(chunk_json: bytes, _user_data: Any) -> int:
            # chunk_json is owned by the library and valid only for this call;
            # ctypes' c_char_p already copied it into a Python bytes object.
            try:
                payload = chunk_json.decode("utf-8")
                verdict = on_chunk(payload if raw else json.loads(payload))
            except BaseException as exc:  # noqa: BLE001 - re-raised below
                raised.append(exc)
                return 1
            return 1 if verdict is False else 0

        # Keep the CFUNCTYPE object alive for the whole call: if it is collected
        # while Go holds the pointer, the next chunk jumps into freed memory.
        cb = ChunkCallback(trampoline)
        err = ctypes.c_char_p()
        rc = self._lib.llmux_stream(
            ctypes.c_uint64(self._handle),
            b"chat",
            _encode(request),
            cb,
            None,
            ctypes.byref(err),
        )
        message = _take_err(self._lib, err)
        if raised:
            raise raised[0]
        if rc != 0:
            raise LLMuxError(message or "llmux_stream failed without a message")

    def stream_iter(
        self, request: Mapping[str, Any], *, raw: bool = False, buffer: int = 64
    ) -> Iterator[Any]:
        """:meth:`stream` as a generator, for ``for chunk in gw.stream_iter(...)``.

        This costs a worker thread, and the cost is inherent: ``llmux_stream``
        blocks and pushes, a generator pulls, and nothing bridges push to pull
        without a second stack. :meth:`stream` is the honest shape of the ABI;
        this is the shape Python code wants. Pick per call site.

        ABANDONING THE GENERATOR REACHES ``llmux_cancel``. ``break`` out of the
        ``for`` loop, an explicit ``.close()``, or simply letting the generator
        be garbage collected all raise ``GeneratorExit`` at the ``yield`` below
        — CPython does this synchronously in the common case, because a ``for``
        loop drops its only reference to the iterator the instant the loop is
        left, and dropping the last reference to a generator closes it
        immediately (see the ``contextlib.closing`` note below for when that
        timing is not good enough). The ``finally`` block below calls
        :meth:`cancel`, and that is the fix for a real bug, not decoration:

        A previous version of this method only set a local flag that the
        callback consulted before forwarding the NEXT chunk to Python. That
        stops chunks from reaching the consumer, but by itself it closes
        nothing: measured against ``sdks/fake-upstream.py`` at 100 ms per
        chunk, a consumer that broke after 3 of 10 words still left the
        upstream generating (and llmux metering) a chunk the consumer never
        asked for, because the local flag can only reject a chunk the
        callback is handed AFTER it is already generated — it cannot reach
        back and stop generation of the one already under way. Only
        ``llmux_cancel`` closes the connection directly instead of waiting
        for the next chunk to arrive and be refused. With ``cancel()`` wired
        into abandonment, the same break leaves the upstream at exactly 3 —
        measured in ``examples/direct_chat.py``.

        ``cancel()`` is PER-HANDLE (see its docstring): abandoning one
        ``stream_iter()`` aborts every OTHER call in flight on this
        ``Gateway`` too, including a second ``stream_iter()`` you meant to
        keep running. Give each stream you might cancel independently its
        own ``Gateway``.

        Do not rely on CPython's immediate refcounting to close this
        generator promptly if you hold a reference to it that outlives the
        loop (a variable assigned outside the ``for``), or on any Python
        implementation other than CPython — PyPy's garbage collector does not
        promise to run a generator's cleanup at any particular time.
        ``contextlib.closing`` makes the cancellation happen exactly where you
        say, on every implementation::

            import contextlib

            with contextlib.closing(gw.stream_iter(request)) as chunks:
                for chunk in chunks:
                    ...
                    break  # cancel() has already run by the time this exits
        """
        done = object()
        chunks: queue.Queue[Any] = queue.Queue(maxsize=max(1, buffer))
        stop = threading.Event()

        def on_chunk(chunk: Any) -> Any:
            # Belt-and-braces: stops a chunk that was already in flight when
            # cancel() ran from being queued for a consumer that is gone. This
            # is NOT what stops the upstream — cancel(), in the `finally`
            # below, is. Without cancel() this flag alone reproduces the bug
            # described above: the consumer stops seeing chunks, the provider
            # keeps generating them.
            if stop.is_set():
                return False
            chunks.put(chunk)
            return None

        def run() -> None:
            try:
                self.stream(request, on_chunk, raw=raw)
                chunks.put(done)
            except BaseException as exc:  # noqa: BLE001 - handed to the consumer
                chunks.put(exc)

        worker = threading.Thread(target=run, name="llmux-stream", daemon=True)
        worker.start()
        try:
            while True:
                item = chunks.get()
                if item is done:
                    return
                if isinstance(item, BaseException):
                    raise item
                yield item
        finally:
            # Reached on every exit from the loop above: it running to
            # completion (`item is done`, where cancel() below is a
            # documented no-op — fact 4 in llmux_cancel's contract), a
            # `break`, an explicit `.close()`, an exception raised out of the
            # loop body, or this generator being garbage collected —
            # GeneratorExit covers the last three. cancel() is safe to call
            # unconditionally: idempotent, and safe to call from THIS thread
            # even while the worker thread above is the one blocked inside
            # `llmux_stream` (see cancel()'s docstring on the GIL — that is
            # exactly the case it is measured against).
            stop.set()
            try:
                self.cancel()
            except Exception:
                # A generator's __del__ can run arbitrarily late — including
                # during interpreter shutdown, from atexit, after `ctypes` or
                # `threading` module globals have already been rebound to
                # None. Anything other than GeneratorExit escaping a
                # generator's close() at that point becomes an "Exception
                # ignored" message on stderr instead of a clean shutdown, and
                # there is nothing left to cancel gracefully by then anyway,
                # so this is deliberately unconditional rather than a narrower
                # except clause naming the handful of AttributeErrors torn-
                # down modules produce.
                pass
            # Unblock a worker parked on a full queue so it can see `stop` and
            # exit even in the rare case cancel() above could not run.
            try:
                while True:
                    chunks.get_nowait()
            except queue.Empty:
                pass
            worker.join(timeout=5)

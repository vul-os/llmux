"""llmux — the LLM multiplexer, in your Python process or beside it.

Two modes, one wire contract. The JSON is the same in both; only the transport
differs.

SIDECAR (the default, and the right default for most Python) — the gateway runs
as a local child process and your existing OpenAI client points at it:

    import llmux
    client = llmux.OpenAI()          # spawns the gateway, returns an OpenAI client
    r = client.chat.completions.create(
        model="anthropic/claude-3-5-sonnet",  # any provider, one client
        messages=[{"role": "user", "content": "hi"}],
    )

DIRECT — libllmux loaded into this interpreter with ctypes. No second process,
no port, no loopback socket:

    from llmux import Gateway

    with Gateway() as gw:
        r = gw.chat({"model": "gpt-4o-mini",
                     "messages": [{"role": "user", "content": "hi"}]})

Direct mode puts the Go runtime inside your interpreter and is NOT fork-safe.
Read README.md — "Which mode" and "The costs" — before choosing it. Set provider
keys via the usual env vars (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY,
...) for either mode.
"""

from ._direct import (
    Gateway,
    LLMuxLibraryError,
    abi_version,
    library_path,
    load_library,
)
from ._sidecar import (
    LLMuxError,
    base_url,
    openai_base_url,
    start,
    stop,
)

__all__ = [
    "AsyncOpenAI",
    "Gateway",
    "LLMuxError",
    "LLMuxLibraryError",
    "OpenAI",
    "abi_version",
    "base_url",
    "library_path",
    "load_library",
    "openai_base_url",
    "start",
    "stop",
]

__version__ = "0.1.0"


def OpenAI(api_key: str = "llmux-local", **kwargs):
    """Return an `openai.OpenAI` client pointed at the local gateway.

    Requires the `openai` package. Equivalent to:
        openai.OpenAI(base_url=llmux.openai_base_url(), api_key=...)
    """
    import openai

    return openai.OpenAI(base_url=openai_base_url(), api_key=api_key, **kwargs)


def AsyncOpenAI(api_key: str = "llmux-local", **kwargs):
    """Return an `openai.AsyncOpenAI` client pointed at the local gateway."""
    import openai

    return openai.AsyncOpenAI(base_url=openai_base_url(), api_key=api_key, **kwargs)

"""llmux — the LLM multiplexer, embedded locally.

    import llmux
    client = llmux.OpenAI()          # spawns the gateway, returns an OpenAI client
    r = client.chat.completions.create(
        model="anthropic/claude-3-5-sonnet",  # any provider, one client
        messages=[{"role": "user", "content": "hi"}],
    )

No server to run: the gateway starts as a local child process and your existing
OpenAI client points at it. Set provider keys via the usual env vars
(OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, ...).
"""

from ._sidecar import (
    LLMuxError,
    base_url,
    openai_base_url,
    start,
    stop,
)

__all__ = [
    "LLMuxError",
    "OpenAI",
    "AsyncOpenAI",
    "base_url",
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

# Routing & reliability

Routes map a client-facing model name to a provider + upstream model, with
fallbacks, retries, and cost-aware selection.

```json
{
  "routes": [
    { "model": "gpt-4o", "provider": "openai" },
    { "model": "claude-3-5-sonnet", "provider": "anthropic", "target_model": "claude-3-5-sonnet-latest" },

    { "model": "smart", "provider": "openai", "target_model": "gpt-4o", "fallbacks": ["anthropic"] },

    {
      "model": "cheapest",
      "strategy": "least-cost",
      "candidates": [
        { "provider": "openai",   "model": "gpt-4o-mini" },
        { "provider": "deepseek", "model": "deepseek-chat" },
        { "provider": "gemini",   "model": "gemini-1.5-flash" }
      ]
    },

    { "model": "*", "provider": "openrouter" }
  ],
  "retry": { "max_retries": 2, "backoff_ms": 200 }
}
```

- **Aliases & prefixes** — call `"smart"`, or `"provider/model"` directly.
- **Fallbacks** — on a retryable error, llmux fails over to the next provider.
- **Retries** — exponential backoff on 429/5xx; spend is charged once.
- **least-cost** — picks the cheapest candidate from the live price catalog,
  with the rest as ordered fallbacks.

Streaming is **byte-identical** to OpenAI's SSE, so every language's stream
parser works unchanged. Client disconnects cancel the upstream request.

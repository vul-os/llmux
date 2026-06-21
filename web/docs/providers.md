# Providers

llmux reaches every provider through one of two mechanisms.

## Passthrough (OpenAI-shaped upstreams)

Most providers already speak the OpenAI schema — llmux just swaps the key and
base URL. These are **stable** and cover the long tail cheaply:

> OpenAI · Azure AI · DeepSeek · Groq · Mistral · Together · Fireworks · xAI ·
> OpenRouter · Ollama · vLLM · Perplexity · Cerebras · SambaNova · …

## Adapters (translating providers)

Providers with their own wire format get a real adapter that translates
requests, responses, streaming, tool-calls, and vision:

| Provider | Type | Stability |
|----------|------|-----------|
| Anthropic | `anthropic` | beta |
| Google Gemini | `gemini` | beta |
| Cohere | `cohere` | experimental |
| AWS Bedrock (Claude) | `bedrock` | experimental |

> **Stability** reflects live verification: `stable` adapters are checked against
> the real API via golden fixtures; `beta`/`experimental` are translated to spec
> but not yet live-verified. `/health` reports each provider's tier.

## Configuration

```json
{
  "providers": [
    { "name": "openai",    "type": "passthrough", "base_url": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY" },
    { "name": "anthropic", "type": "anthropic",   "api_key_env": "ANTHROPIC_API_KEY" },
    { "name": "bedrock",   "type": "bedrock",     "headers": { "region": "us-east-1" } }
  ]
}
```

With provider env vars set, llmux **auto-detects** providers — no config file
needed to get started.

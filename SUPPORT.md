# Provider support & stability

Honest status. A provider is **stable** only once it is verified against the
*real* API via golden fixtures + the live smoke suite (`make smoke`). Until then
it is **beta** (real translation, unit-tested against mocks) or **experimental**
(written to the documented spec, unverified). `/health` reports each configured
provider's stability.

| Provider | Type | Stability | Chat | Stream | Tools | Vision | Embeddings | Live-verified |
|----------|------|-----------|:----:|:------:|:-----:|:------:|:----------:|:-------------:|
| OpenAI / DeepSeek / Groq / Mistral / Together / Fireworks / xAI / OpenRouter / Ollama / vLLM | `passthrough` | **stable** | ✅ | ✅ | ✅ | ✅ | ✅ | ⏳ |
| Anthropic | `anthropic` | **beta** | ✅ | ✅ | ✅ | ✅ | — | ❌ |
| Google Gemini | `gemini` | **beta** | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| Cohere | `cohere` | **experimental** | ✅ | ✅ | ✅ | — | ✅ | ❌ |
| AWS Bedrock (Anthropic) | `bedrock` | **experimental** | ✅ | synth | ✅ | ✅ | — | ❌ |

Legend: ✅ implemented · synth = synthesized (non-native) · ⏳ pending live run ·
❌ not yet live-verified.

## Promotion criteria (experimental → beta → stable)
1. **beta:** full translation implemented + unit tests against recorded-shape mocks.
2. **stable:** golden fixtures recorded from the real API (`make record`) **and**
   the live smoke suite passes (`make smoke`) for: non-stream, stream, tools,
   vision (if supported), error mapping, and usage.

> Recording/verifying requires real provider keys. Run `make record` once with
> keys to populate fixtures, then `make smoke` in CI to keep providers honest.

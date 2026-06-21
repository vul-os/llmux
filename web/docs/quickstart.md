# Quickstart

llmux is a single Go binary that speaks the **OpenAI-compatible HTTP API** and
routes to any provider behind it. Run it, point any OpenAI client at it, done.

## Run the gateway

```bash
# build the binary (or grab a release)
make build

# providers are auto-detected from environment variables
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...

./dist/llmux            # listening on http://localhost:4000
```

## Call it from any language

The model string selects the provider — your existing OpenAI client doesn't
change except for `base_url`.

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:4000/v1", api_key="sk-…")

client.chat.completions.create(
    model="anthropic/claude-3-5-sonnet",   # any provider, one client
    messages=[{"role": "user", "content": "hello"}],
)
```

## Embed it locally — no server to run

```bash
pip install llmux      # or: npm install llmux
```

```python
import llmux
client = llmux.OpenAI()     # spawns the gateway as a local sidecar
```

In Go, llmux runs **in-process** — no subprocess at all:

```go
local, _ := llmux.Start(llmux.Options{})
defer local.Close()
// point any OpenAI Go client at local.OpenAIBaseURL()
```

## Selecting a model

- a configured **alias** — `"claude-3-5-sonnet"`
- a **provider/model** prefix — `"anthropic/claude-3-5-sonnet"`
- a **strategy** alias — `"cheapest"` (least-cost across candidates)

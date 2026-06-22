# llmux (Ruby)

Use llmux **locally** from Ruby. The gem bundles the gateway binary, starts it
on a local port, and hands your existing OpenAI client a `base_url`.

```ruby
require "llmux"

# Just the URL — point any HTTP/OpenAI client at it:
base = Llmux.base_url          # => "http://127.0.0.1:<port>"
v1   = Llmux.openai_base_url   # => "http://127.0.0.1:<port>/v1"

# Or a configured ruby-openai client (optional `ruby-openai` gem):
client = Llmux.openai
resp = client.chat(parameters: {
  model: "anthropic/claude-3-5-sonnet",
  messages: [{ role: "user", content: "hi" }],
})
```

The sidecar starts lazily on first use, is reused (singleton), and is terminated
at process exit.

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the gem's `bin/`:

```sh
go build -o sdks/ruby/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

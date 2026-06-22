# llmux (Elixir)

Use llmux **locally** from Elixir. The package bundles the gateway binary,
starts it on a local port via an Erlang `Port` (managed by a singleton
GenServer, `Llmux.Sidecar`), and hands your OpenAI-compatible client a
`base_url`.

```elixir
{:ok, base} = Llmux.base_url()         # "http://127.0.0.1:<port>"
{:ok, v1} = Llmux.openai_base_url()    # "http://127.0.0.1:<port>/v1"
```

The sidecar starts lazily on first use, is reused (one GenServer process), and
is terminated when the GenServer stops — including BEAM shutdown, which closes
the Port and reaps the child.

> Why Port and not `System.cmd`: `System.cmd/3` blocks until the process exits,
> which is wrong for a long-lived sidecar. A `Port` gives us a non-blocking,
> supervised handle with exit notifications and automatic teardown — the
> idiomatic fit for the contract.

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `priv/bin/llmux` (`priv/bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the package's `priv/bin/`:

```sh
go build -o sdks/elixir/priv/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

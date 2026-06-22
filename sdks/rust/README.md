# llmux (Rust)

Use llmux **locally** from Rust. The crate bundles the gateway binary, starts it
on a local port via `std::process`, and hands your OpenAI-compatible client a
`base_url`. The health poll uses a tiny `std::net` GET — no HTTP dependency.

```rust
let base = llmux::base_url()?;          // http://127.0.0.1:<port>
let v1 = llmux::openai_base_url()?;     // http://127.0.0.1:<port>/v1
# Ok::<(), llmux::Error>(())
```

With the `async-openai` feature, get a configured client:

```toml
[dependencies]
llmux = { version = "0.1", features = ["async-openai"] }
```

```rust
let client = llmux::openai_client()?;   // async_openai::Client pointed at llmux
```

The sidecar starts lazily on first use, is reused (singleton, `Mutex`-guarded),
and is killed by [`llmux::stop`]. Call it before exit if you want explicit
teardown.

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the crate's `bin/`:

```sh
go build -o sdks/rust/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

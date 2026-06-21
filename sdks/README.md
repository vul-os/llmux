# llmux language packages

Thin, native packages that let you use llmux **locally** in any language — no
server to run. They bundle the gateway binary and manage it for you.

The architecture is deliberate: one Go binary is **both** the hosted server and
the locally-embedded sidecar. The packages here are tiny wrappers (~one file
each) that start the binary on a local port and hand you a `base_url` for your
existing OpenAI client. Streaming works natively in every language because each
just reads its own local socket — no FFI, no per-language stream glue.

| Package | Mechanism | Streaming |
|---------|-----------|-----------|
| **python** | spawns the bundled binary on `127.0.0.1:<port>` | native (your OpenAI client) |
| **node** | spawns the bundled binary on `127.0.0.1:<port>` | native |
| **go** | runs the gateway **in-process** (imports `core/`) | native |

## Binary distribution

For local development, run `make sdk-bins` to build the binary into each
package's `bin/` directory. Real releases produce per-OS/arch binaries in CI and
ship them inside platform wheels (Python) and `optionalDependencies` (npm); the
Go package needs no binary because it embeds the gateway directly.

Override the binary path anytime with `LLMUX_BINARY=/path/to/llmux`.

## Provider keys

All packages inherit provider API keys from the environment
(`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …), so the embedded
gateway auto-detects providers exactly like the standalone binary.

## Proven

Each package has been run end-to-end making a real chat completion through llmux:
Python sidecar, Node sidecar, and Go in-process — all from this one Go codebase.

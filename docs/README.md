# llmux documentation

Everything you need to import llmux as a Go library, or run, configure, and
operate it as a server.

## Start

| Guide | What's inside |
|---|---|
| [Quickstarts](quickstarts.md) | Five five-minute tracks: point a client at a gateway, ship an app, drop the child process too, embed in Go, self-host |
| [Getting started](GETTING-STARTED.md) | Deploy the gateway, connect providers, auth and keys, point Vulos OS at it |
| [Client examples](client-examples.md) | Copy-paste requests in curl and 17+ languages |

## Embed

| Guide | What's inside |
|---|---|
| [Choosing a mode](choosing-a-mode.md) | Server vs sidecar vs Go library vs C shared library — the trade-offs, decided in five minutes |
| [Embedding llmux in Go](embedding.md) | The `core/gateway` API in full: construction, dispatch, `Authorize`/`release`, seams, and what `New` does on its own |
| [The C ABI](c-abi.md) | Seven functions, the ownership rules, the costs, the honest platform matrix, and the thirteen bindings that already exist |
| [Language packages](sdks.md) | All **fifteen**: which mechanism each defaults to and why, a working first call per language, and how each is tested |

## Understand and operate

| Guide | What's inside |
|---|---|
| [Architecture](architecture.md) | How the gateway is laid out, and the sovereignty gate |
| [Configuration](configuration.md) | Config file, environment variables, and overrides |
| [Control-plane seam](control-plane.md) | Optional centralized billing & entitlements |
| [API reference](api.md) | Endpoints, auth, errors, and cost |
| [LLM access: BYOK vs central](LLM-ACCESS.md) | Per-account own-key vs central metered keys, billing, the product consumption contract |
| [Admin guide](ADMIN-GUIDE.md) | Budgets, rate limits, cost accounting, model routing in depth, the dashboard, logging/privacy posture |
| [Operations](operations.md) | Building, testing, and self-hosting |
| [Troubleshooting](TROUBLESHOOTING.md) | Symptom-by-symptom fixes, including the embedded and C-ABI paths |

## Reference

| Document | Purpose |
|---|---|
| [`llmux.example.json`](../llmux.example.json) | Fully commented example config |
| [`ffi/include/llmux.h`](../ffi/include/llmux.h) | The normative C header |
| [`ffi/README.md`](../ffi/README.md) | C-ABI implementation notes, build detail, test strategy |
| [`sdks/README.md`](../sdks/README.md) | The package index: each language's modes, default and streaming shape |
| [Hardening](../HARDENING.md) | Production security posture |
| [Provider parity](parity.md) | Per-provider feature support matrix |
| [Testing](../TESTING.md) | Test suites and how to run them |
| [Security policy](../SECURITY.md) | Reporting vulnerabilities |
| [Roadmap](../ROADMAP.md) | What's planned |
| [Changelog](../CHANGELOG.md) | What's shipped |
| [Support](../SUPPORT.md) | Getting help |

> These guides are also published at [vulos.org/projects/llmux/docs.html](https://vulos.org/projects/llmux/docs.html)
> — the running gateway's own `/ui` is the admin dashboard only, with no
> separate docs viewer embedded in the binary.

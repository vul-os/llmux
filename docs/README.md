# llmux documentation

Everything you need to run, configure, and operate llmux.

## Guides

| Guide | What's inside |
|---|---|
| [Getting started](GETTING-STARTED.md) | Deploy the gateway, connect providers, auth and keys, point Vulos OS at it |
| [Client examples](client-examples.md) | Copy-paste requests in curl and 17+ languages, plus embedding llmux locally with no server to run |
| [API reference](api.md) | Endpoints, auth, errors, and cost |
| [Configuration](configuration.md) | Config file, environment variables, and overrides |
| [Architecture](architecture.md) | How the gateway is laid out, and the sovereignty gate |
| [Admin guide](ADMIN-GUIDE.md) | Budgets, rate limits, cost accounting, model routing in depth, the dashboard, logging/privacy posture |
| [LLM access: BYOK vs central](LLM-ACCESS.md) | Per-account own-key vs central metered keys, billing, the product consumption contract |
| [Control-plane seam](control-plane.md) | Optional centralized billing & entitlements |
| [Operations](operations.md) | Building, testing, and self-hosting |
| [Troubleshooting](TROUBLESHOOTING.md) | Symptom-by-symptom fixes |

## Reference

| Document | Purpose |
|---|---|
| [`llmux.example.json`](../llmux.example.json) | Fully commented example config |
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

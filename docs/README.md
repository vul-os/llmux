# llmux documentation

Everything you need to run, configure, and operate llmux.

## Guides

| Guide | What's inside |
|---|---|
| [Quickstart](../web/docs/quickstart.md) | Run the gateway and make your first request |
| [API reference](api.md) | Endpoints, auth, errors, and cost |
| [Configuration](configuration.md) | Config file, environment variables, and overrides |
| [Routing & reliability](../web/docs/routing.md) | Aliases, prefixes, fallback chains, least-cost selection |
| [Providers](../web/docs/providers.md) | Native adapters vs. passthrough, and what each supports |
| [Pricing & cost](../web/docs/pricing.md) | The live catalog, precedence, and per-request cost |
| [Dashboard & ops](../web/docs/dashboard.md) | The embedded admin UI at `/ui` |
| [Architecture](architecture.md) | How the gateway is laid out and why |
| [Control-plane seam](control-plane.md) | Optional centralized billing & entitlements |
| [Operations](operations.md) | Building, testing, and self-hosting |

## Reference

| Document | Purpose |
|---|---|
| [`llmux.example.json`](../llmux.example.json) | Fully commented example config |
| [Hardening](../HARDENING.md) | Production security posture |
| [Provider parity](../PARITY.md) | Per-provider feature support matrix |
| [Testing](../TESTING.md) | Test suites and how to run them |
| [Security policy](../SECURITY.md) | Reporting vulnerabilities |
| [Roadmap](../roadmap.md) | What's planned |
| [Support](../SUPPORT.md) | Getting help |

> The same guides ship **inside the binary** and are served at `/ui/docs` in the
> running gateway.

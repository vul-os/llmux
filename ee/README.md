# `ee/` — reserved, empty

**There is no code here, and nothing in the repo depends on this directory.**

It is a reserved namespace for a possible future open-core split. Everything
llmux does today lives in `core/`, `cmd/`, and the optional `integration/cp`
seam, and all of it is self-hostable with no paid tier and no feature gate.

The whole repository — this directory included — is licensed
[MIT](../LICENSE-MIT) **OR** [Apache-2.0](../LICENSE-APACHE), at your option.

What might eventually land here is tracked, as a plan and not as a shipped
feature, in [ROADMAP.md](../ROADMAP.md). Nothing on that list exists yet. In
particular llmux has **no accounts, no login, and no SSO** of any kind today:
the gateway authenticates with operator-issued bearer tokens only, and is
configured by endpoint — see [docs/api.md](../docs/api.md#authentication).

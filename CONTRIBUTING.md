# Contributing to llmux

Thanks for helping build a self-hosted OpenAI-compatible gateway that never
sends inference off-box without an explicit, logged opt-in. Contributions are
under the project's dual licence, [MIT](LICENSE-MIT) OR
[Apache-2.0](LICENSE-APACHE).

## Dev environment setup

Requirements: Go 1.25+ (see `toolchain` in `go.mod`). Nothing else — there is
no Node toolchain anywhere in this repo.

```bash
go build ./...
go test ./...
```

The embedded admin console is `web/ui.html` — one hand-written file with
inline CSS/JS, no build step. Edit it directly and `go build` picks it up.

## Branch and PR conventions

- Branch off `main`. One logical change per PR; keep diffs reviewable.
- Conventional Commits welcome, not required (`feat(router): ...`,
  `fix(bedrock): ...`, `chore: ...`).

## Before opening a PR

```bash
make gates   # gofmt scope + release-verifier failure matrix
make test    # go test -race ./...
make vet     # go vet ./...
```

`make gates` is deliberately fail-closed rather than skip-on-missing-tool — see
the comments in `scripts/gofmt-check.sh` if a gate refuses to run instead of
passing quietly. There used to be a second gate here (`web-dist-check`,
`scripts/check-web-dist.sh`) guarding a committed `web/dist` bundle against
drift from its `web/src`; both are gone along with the build step, since
`web/ui.html` is hand-written and has no bundle to drift. If you touched
`web/ui.html`, `go build` alone is enough — `web/embed_test.go` and
`core/server/ui_test.go` cover it.

See [TESTING.md](TESTING.md) for the full layer breakdown (unit, contract,
gated Postgres/Redis integration, conformance fixtures, live smoke, fuzz, and
the structural guards that read source instead of running it).

## Non-negotiables

From [ROADMAP.md](ROADMAP.md):

- OpenAI HTTP compatibility is sacred — never break the client contract.
- `core/` stays free and self-hostable forever.
- No provider detail leaks past the gateway boundary.
- Inference stays on-box by default; nothing egresses silently — the
  sovereignty gate (`core/sovereign`, see [docs/architecture.md](docs/architecture.md))
  is not optional. A new dispatch path must call `enforceSovereignty` before
  any network call, or `TestNoUngatedProviderDispatch` will fail.
- The pricing catalog stays open and auto-synced.

## Provider adapters

New or changed adapters follow the stability ladder in
[SUPPORT.md](SUPPORT.md): `experimental` (written to spec, unverified) →
`beta` (unit-tested against recorded-shape mocks) → `stable` (golden
fixtures from the real API via `make record`, plus `make smoke` passing).
Don't mark a provider `stable` without both.

## Reporting security issues

Do not file public issues for vulnerabilities — see [SECURITY.md](SECURITY.md).

## Licensing

By contributing, you agree your contributions are licensed under **MIT OR
Apache-2.0** (see [LICENSE-MIT](LICENSE-MIT) and
[LICENSE-APACHE](LICENSE-APACHE)). No CLA required.

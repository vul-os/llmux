# Operations

Building, testing, and running llmux in production.

## Build & run

**Prerequisites:** Go 1.25+, Node 18+ (only to rebuild the embedded web UI), and
at least one provider API key.

```bash
make web        # rebuild the embedded React/Vite admin UI into web/dist
make build      # build the gateway binary (embeds web/dist)
make run        # build and run on :4000
make docker     # build the Docker image
```

The web app uses `.jsx` only (never `.tsx`).

## Testing

```bash
make test       # all Go tests with -race
make vet        # static analysis
make cover      # coverage summary
```

Integration tests against Postgres/Redis activate when `LLMUX_TEST_POSTGRES` /
`LLMUX_TEST_REDIS` are set. Provider conformance fixtures and the live smoke
suite (`make record`, `make smoke`) require real provider keys. Full details in
[TESTING.md](../TESTING.md).

## Self-hosting

llmux is a single binary with no required runtime dependencies — drop it on a
host (or use the included [`Dockerfile`](../Dockerfile)), point it at a config,
and set your provider keys.

Add Postgres and Redis when you scale to multiple replicas so keys, spend, rate
limits, and cache stay consistent (see [Configuration](configuration.md)). For
production security posture — auth, limits, timeouts, network exposure — follow
[HARDENING.md](../HARDENING.md).

## CLI

Beyond serving, the binary exposes inspection subcommands:

```bash
./dist/llmux models       # models with pricing + context window
./dist/llmux catalog      # price catalog count and last sync time
./dist/llmux keys         # virtual keys: budget, spend, rpm
```

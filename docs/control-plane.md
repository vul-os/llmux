# Control-plane billing seam

llmux runs fully standalone. The control-plane seam is **entirely opt-in**: it
lets you centralize identity, budget, and usage reporting across a fleet of
gateways when you want them — and stays completely out of the way when you don't.

## Standalone (default)

Leave it off and llmux uses the static virtual keys from your config. No external
service, no network calls beyond your providers.

## Centralized

To resolve identity, gate budget, and report usage centrally, set:

```bash
export LLMUX_CP_URL=https://control-plane.example.com
export LLMUX_CP_SECRET=...        # shared secret
```

(or the equivalent `cp` block in the JSON config). The gateway then:

1. **Resolves identity** for each request against the control plane.
2. **Gates budget** before dispatching to a provider.
3. **Reports usage** back, authenticated with an `X-Relay-Auth` shared secret.

## Degraded mode

If the control plane is unreachable, llmux fails safe to a conservative per-account
rate cap rather than blocking all traffic:

| Setting | Effect |
|---|---|
| `cp_degraded_rpm` | Per-account requests/minute allowed while the control plane is down |
| `cp_degraded_fail_open` | Allow requests through unmetered if you accept the spend risk |

## Usage delivery durability

Reported usage is sent to cp by a bounded in-memory retry queue (fast path).
To also survive an extended cp outage or a process crash, set a durable spool
file:

| Setting | Effect |
|---|---|
| `cp_usage_spool_path` / `LLMUX_CP_USAGE_SPOOL_PATH` | Persists every not-yet-acknowledged usage record to this file. A background reconciler retries anything still there (e.g. left over from a crash, or a record the fast path gave up on) until cp acknowledges it. Empty = in-memory-only (records that outlive the fast path's retries or a crash are lost from cp's perspective). |

cp already dedupes every record by its idempotency key, so reconciled retries
are always billed at most once.

## Isolation

The control-plane adapter lives in `integration/cp` and is wired only by
`cmd/llmux`. The `core` gateway never depends on it — see
[Architecture](architecture.md).

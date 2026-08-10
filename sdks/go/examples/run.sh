#!/usr/bin/env bash
# Run the Go examples offline, with no provider keys and no network.
#
#   ./sdks/go/examples/run.sh direct
#   ./sdks/go/examples/run.sh sidecar
#   ./sdks/go/examples/run.sh cancel
#   ./sdks/go/examples/run.sh            # all three
#
# direct and sidecar boot ffi/fakeupstream — the same OpenAI-compatible fake
# the C smoke test and the latency benchmark use — and hand the example the
# configuration that fake prints for itself. Nothing here composes llmux
# config JSON by hand: the day the schema changes, a hand-rolled literal in a
# runner script is how the examples start testing a fiction.
#
# cancel boots a different fake, sdks/fake-upstream.py, because ffi/fakeupstream
# has neither a per-chunk delay (nothing to cancel in the middle of) nor a
# /generated counter (no way to tell whether cancelling actually stopped the
# upstream, versus just stopping delivery to this process). It needs python3;
# if that is not on PATH the direct/sidecar sections above still run, and this
# one is skipped with a clear message rather than failing the whole script.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
work="$(mktemp -d)"
pids=()

cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

echo "==> building fakeupstream (module github.com/vul-os/llmux-ffi)"
(cd "$repo/ffi" && go build -o "$work/fakeupstream" ./fakeupstream)

echo "==> starting fakeupstream"
"$work/fakeupstream" -text "one two three four" > "$work/up.txt" 2>&1 &
pids+=($!)
for _ in $(seq 1 100); do
  grep -q '^CONFIG ' "$work/up.txt" 2>/dev/null && break
  sleep 0.05
done
grep -q '^CONFIG ' "$work/up.txt" || { echo "fakeupstream never printed CONFIG"; cat "$work/up.txt"; exit 1; }

config_json="$(sed -n 's/^CONFIG //p' "$work/up.txt")"
printf '%s\n' "$config_json" > "$work/llmux.json"
echo "    $(sed -n 's/^URL //p' "$work/up.txt")"

want="${1:-both}"

if [[ "$want" == "direct" || "$want" == "both" ]]; then
  echo
  echo "==> direct (in-process, no port)"
  (cd "$repo" && go run ./sdks/go/examples/direct -config "$config_json" -model demo)
fi

if [[ "$want" == "sidecar" || "$want" == "both" ]]; then
  echo
  echo "==> building the llmux binary"
  (cd "$repo" && go build -o "$work/llmux" ./cmd/llmux)
  echo "==> sidecar (child process over HTTP)"
  (cd "$repo" && go run ./sdks/go/examples/sidecar \
    -bin "$work/llmux" -config "$work/llmux.json" -model demo)
fi

if [[ "$want" == "cancel" || "$want" == "both" ]]; then
  echo
  if ! command -v python3 >/dev/null 2>&1; then
    echo "==> cancel demo skipped: no python3 on PATH"
  else
    echo "==> starting sdks/fake-upstream.py (100ms/chunk, so there is something to cancel)"
    python3 "$repo/sdks/fake-upstream.py" \
      --text "one two three four five six seven eight nine ten" \
      --chunk-delay-ms 100 > "$work/cancel-up.txt" 2>&1 &
    pids+=($!)
    for _ in $(seq 1 100); do
      grep -q '^CONFIG ' "$work/cancel-up.txt" 2>/dev/null && break
      sleep 0.05
    done
    grep -q '^CONFIG ' "$work/cancel-up.txt" || {
      echo "fake-upstream.py never printed CONFIG"; cat "$work/cancel-up.txt"; exit 1;
    }
    cancel_config="$(sed -n 's/^CONFIG //p' "$work/cancel-up.txt")"
    echo "    $(sed -n 's/^URL //p' "$work/cancel-up.txt")"

    echo "==> direct, context cancellation (cancel after 3 delivered chunks)"
    (cd "$repo" && go run ./sdks/go/examples/direct -config "$cancel_config" -model demo -cancel-demo)

    # The example prints the counter's address but does not read it. It embeds
    # the gateway and dials nothing at all — that is the contrast the
    # direct/sidecar pair exists to draw, and core/sovereign's egress guard
    # holds it to that: the entry permitting the sidecar example's HTTP cites
    # this file's silence as the reason it is safe. So the out-of-band read
    # happens here, in the runner, where it costs the example nothing.
    cancel_url="$(sed -n 's/^URL //p' "$work/cancel-up.txt")"
    echo "upstream generated: $(curl -fsS "$cancel_url/generated")"
  fi
fi

#!/usr/bin/env bash
# Run the Go examples offline, with no provider keys and no network.
#
#   ./sdks/go/examples/run.sh direct
#   ./sdks/go/examples/run.sh sidecar
#   ./sdks/go/examples/run.sh            # both
#
# It boots ffi/fakeupstream — the same OpenAI-compatible fake the C smoke test
# and the latency benchmark use — and hands the example the configuration that
# fake prints for itself. Nothing here composes llmux config JSON by hand: the
# day the schema changes, a hand-rolled literal in a runner script is how the
# examples start testing a fiction.
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

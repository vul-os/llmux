#!/usr/bin/env bash
# Run the Rust examples offline, with no provider keys and no network.
#
#   ./sdks/rust/examples/run.sh direct
#   ./sdks/rust/examples/run.sh sidecar
#   ./sdks/rust/examples/run.sh            # both
#
# Boots ffi/fakeupstream — the same OpenAI-compatible fake the C smoke test and
# the latency benchmark use — and hands each example the configuration that
# fake prints for itself, so no llmux config JSON is hand-rolled here.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
crate="$(cd "$here/.." && pwd)"
repo="$(cd "$crate/../.." && pwd)"
work="$(mktemp -d)"
pids=()

cleanup() {
  for pid in "${pids[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$work"
}
trap cleanup EXIT

echo "==> building fakeupstream"
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
  lib="$repo/dist/ffi/darwin_arm64/libllmux.dylib"
  [[ -f "$lib" ]] || lib="$repo/dist/ffi/linux_arm64/libllmux.so"
  if [[ ! -f "$lib" ]]; then
    echo "==> direct SKIPPED: no libllmux built. Run scripts/build-ffi.sh first."
  else
    echo
    echo "==> direct (in-process, C ABI)"
    (cd "$crate" && LLMUX_LIBRARY="$lib" LLMUX_CONFIG_JSON="$config_json" LLMUX_MODEL=demo \
      cargo run --quiet --example direct)
  fi
fi

if [[ "$want" == "sidecar" || "$want" == "both" ]]; then
  echo
  echo "==> building the llmux binary"
  (cd "$repo" && go build -o "$work/llmux" ./cmd/llmux)
  echo "==> sidecar (child process over HTTP)"
  (cd "$crate" && LLMUX_BINARY="$work/llmux" LLMUX_CONFIG="$work/llmux.json" LLMUX_MODEL=demo \
    cargo run --quiet --example sidecar)
fi

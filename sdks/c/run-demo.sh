#!/usr/bin/env bash
#
# run-demo.sh — build and run the C examples end to end, with no provider
# account and no network.
#
# It starts ffi/fakeupstream — the same OpenAI-compatible fake the Go tests, the
# C smoke test and the latency benchmark drive — and hands its configuration to
# both examples: as a JSON argument to the direct one, and as a config file to
# the sidecar's binary. One fake, one config document, two doors.
#
# Usage:
#   ./run-demo.sh                 # both
#   ./run-demo.sh direct_chat     # one
#
# Needs a Go toolchain to build the fake upstream, and will build the shared
# library and the llmux binary if they are not already in dist/.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
case "${goos}" in
  darwin) libname="libllmux.dylib" ;;
  *) libname="libllmux.so" ;;
esac

libdir="${LLMUX_LIB_DIR:-${root}/dist/ffi/${goos}_${goarch}}"
if [ ! -f "${libdir}/${libname}" ]; then
  echo "==> building ${libname} (scripts/build-ffi.sh)"
  "${root}/scripts/build-ffi.sh" >/dev/null
fi

bin="${LLMUX_BINARY:-${root}/dist/llmux}"
if [ ! -x "${bin}" ]; then
  echo "==> building the llmux binary"
  (cd "${root}" && go build -o "${bin}" ./cmd/llmux)
fi
export LLMUX_BINARY="${bin}"

tmp="$(mktemp -d)"
cleanup() {
  [ -n "${fake_pid:-}" ] && kill "${fake_pid}" 2>/dev/null || true
  rm -rf "${tmp}"
}
trap cleanup EXIT

echo "==> starting the fake upstream (ffi/fakeupstream)"
(cd "${root}/ffi" && go build -o "${tmp}/fakeupstream" ./fakeupstream)
"${tmp}/fakeupstream" >"${tmp}/fake.out" 2>"${tmp}/fake.err" &
fake_pid=$!
for _ in $(seq 1 100); do
  grep -q '^CONFIG ' "${tmp}/fake.out" 2>/dev/null && break
  sleep 0.05
done
if ! grep -q '^CONFIG ' "${tmp}/fake.out"; then
  echo "run-demo: the fake upstream never printed a CONFIG line" >&2
  cat "${tmp}/fake.err" >&2
  exit 1
fi

config="$(sed -n 's/^CONFIG //p' "${tmp}/fake.out")"
printf '%s' "${config}" >"${tmp}/llmux.json"
export LLMUX_CONFIG="${tmp}/llmux.json"     # read by the binary (sidecar mode)
export LLMUX_CONFIG_JSON="${config}"        # read by the example (direct mode)
export LLMUX_DEMO_MODEL=demo

echo "==> building the examples"
make -C "${here}" LLMUX_LIB_DIR="${libdir}" all >/dev/null

examples=("$@")
if [ ${#examples[@]} -eq 0 ]; then
  examples=(direct_chat sidecar_chat)
fi

echo "    upstream:  $(sed -n 's/^URL //p' "${tmp}/fake.out")"
echo "    library:   ${libdir}/${libname}"
echo "    binary:    ${LLMUX_BINARY}"

for example in "${examples[@]}"; do
  echo
  echo "=============================================================="
  echo "  ${example}"
  echo "=============================================================="
  "${here}/${example}"
done

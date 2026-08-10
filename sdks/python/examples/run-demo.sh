#!/usr/bin/env bash
#
# run-demo.sh — run the Python examples end to end with no provider account and
# no network.
#
# It starts ffi/fakeupstream, which is the SAME OpenAI-compatible fake the Go
# tests, the C smoke test and the latency benchmark use, and which prints the
# llmux configuration that routes at it. Reusing that instead of hand-rolling a
# fake here is deliberate: two fakes drift, and then an example passes against a
# fiction the tests do not share.
#
# Usage:
#   examples/run-demo.sh                 # every example
#   examples/run-demo.sh direct_chat.py  # one of them
#
# Requirements: a Go toolchain (to build the fake upstream and, if you have not
# built them, the library and binary), and:
#   scripts/build-ffi.sh                          -> dist/ffi/<goos>_<goarch>/
#   go build -o dist/llmux ./cmd/llmux            -> the sidecar binary
# Both are found automatically if present; the script builds them if not.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pkg="$(cd "${here}/.." && pwd)"
root="$(cd "${pkg}/../.." && pwd)"
python="${PYTHON:-python3}"

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
case "${goos}" in
  darwin) libname="libllmux.dylib" ;;
  windows) libname="llmux.dll" ;;
  *) libname="libllmux.so" ;;
esac

# --- the shared library (direct mode) --------------------------------------
lib="${LLMUX_LIBRARY:-${root}/dist/ffi/${goos}_${goarch}/${libname}}"
if [ ! -f "${lib}" ]; then
  echo "==> building ${libname} (scripts/build-ffi.sh)"
  "${root}/scripts/build-ffi.sh" >/dev/null
fi
export LLMUX_LIBRARY="${lib}"

# --- the binary (sidecar mode) ---------------------------------------------
bin="${LLMUX_BINARY:-${root}/dist/llmux}"
if [ ! -x "${bin}" ]; then
  echo "==> building the llmux binary"
  (cd "${root}" && go build -o "${bin}" ./cmd/llmux)
fi
export LLMUX_BINARY="${bin}"

# --- the fake upstream ------------------------------------------------------
tmp="$(mktemp -d)"
cleanup() {
  [ -n "${fake_pid:-}" ] && kill "${fake_pid}" 2>/dev/null || true
  [ -n "${cancel_pid:-}" ] && kill "${cancel_pid}" 2>/dev/null || true
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

# Direct mode takes the configuration as a JSON document; the sidecar binary
# reads it from a file. Same document, two doors.
LLMUX_CONFIG_JSON="$(sed -n 's/^CONFIG //p' "${tmp}/fake.out")"
export LLMUX_CONFIG_JSON
printf '%s' "${LLMUX_CONFIG_JSON}" >"${tmp}/llmux.json"
export LLMUX_CONFIG="${tmp}/llmux.json"
export LLMUX_DEMO_MODEL=demo

echo "    upstream:  $(sed -n 's/^URL //p' "${tmp}/fake.out")"
echo "    library:   ${LLMUX_LIBRARY}"
echo "    binary:    ${LLMUX_BINARY}"

# --- the timed fake upstream, for direct_chat.py's cancellation demo -------
# ffi/fakeupstream above answers instantly and counts nothing, which is fine
# for models/chat/stream but cannot show whether a cancelled stream_iter()
# actually stopped the PROVIDER rather than just stopping delivery to this
# process. That needs a provider slow enough to interrupt mid-stream and
# honest enough to report what it wrote: sdks/fake-upstream.py, stdlib
# Python, no Go toolchain, shared with every other language's SDK for this
# exact measurement.
echo "==> starting the timed fake upstream for the cancellation demo (sdks/fake-upstream.py)"
"${python}" "${root}/sdks/fake-upstream.py" \
  --chunk-delay-ms 100 \
  --text "one two three four five six seven eight nine ten" \
  >"${tmp}/cancel.out" 2>"${tmp}/cancel.err" &
cancel_pid=$!

for _ in $(seq 1 100); do
  grep -q '^CONFIG ' "${tmp}/cancel.out" 2>/dev/null && break
  sleep 0.05
done
if ! grep -q '^CONFIG ' "${tmp}/cancel.out"; then
  echo "run-demo: the timed fake upstream never printed a CONFIG line" >&2
  cat "${tmp}/cancel.err" >&2
  exit 1
fi

export LLMUX_CANCEL_CONFIG_JSON="$(sed -n 's/^CONFIG //p' "${tmp}/cancel.out")"
export LLMUX_CANCEL_UPSTREAM_URL="$(sed -n 's/^URL //p' "${tmp}/cancel.out")"
echo "    cancel upstream: ${LLMUX_CANCEL_UPSTREAM_URL} (100ms/chunk, GET /generated)"

examples=("$@")
if [ ${#examples[@]} -eq 0 ]; then
  examples=(direct_chat.py sidecar_chat.py fork_hazard.py)
fi

for example in "${examples[@]}"; do
  echo
  echo "=============================================================="
  echo "  ${example}"
  echo "=============================================================="
  (cd "${pkg}" && "${python}" "examples/${example}")
done

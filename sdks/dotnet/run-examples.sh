#!/usr/bin/env bash
#
# run-examples.sh — compile and RUN both .NET examples, direct and sidecar.
#
# Stands up everything they need:
#   * the shared library     (scripts/build-ffi.sh)   for the direct example
#   * the gateway binary     (go build ./cmd/llmux)   for the sidecar example
#   * a fake OpenAI upstream (ffi/fakeupstream)       for both, so neither
#     needs an API key or a network
#
# Fails closed: no dotnet, a library that would not build, or an example that
# exits non-zero is a FAILURE, never a skip.
#
# Usage:  sdks/dotnet/run-examples.sh [direct|sidecar]
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
which="${1:-both}"

export DOTNET_CLI_TELEMETRY_OPTOUT=1
export DOTNET_NOLOGO=1

fail() { echo "run-examples: FAIL — $*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || fail "go is not on PATH"
command -v dotnet >/dev/null 2>&1 || fail "dotnet is not on PATH"
echo "run-examples: dotnet $(dotnet --version), go $(go version | awk '{print $3}')"

tmp="$(mktemp -d)"
fake_pid=""
cleanup() {
  if [ -n "${fake_pid}" ] && kill -0 "${fake_pid}" 2>/dev/null; then
    kill "${fake_pid}" 2>/dev/null || true
    wait "${fake_pid}" 2>/dev/null || true
  fi
  rm -rf "${tmp}"
}
trap cleanup EXIT

# --- the shared library ------------------------------------------------------
goos="$(go env GOOS)"; goarch="$(go env GOARCH)"
case "${goos}" in
  darwin) libfile="libllmux.dylib" ;;
  windows) libfile="llmux.dll" ;;
  *) libfile="libllmux.so" ;;
esac
libpath="${root}/dist/ffi/${goos}_${goarch}/${libfile}"
if [ ! -f "${libpath}" ]; then
  echo "run-examples: building ${libfile}…"
  "${root}/scripts/build-ffi.sh" >"${tmp}/build.log" 2>&1 \
    || { cat "${tmp}/build.log" >&2; fail "the shared library did not build"; }
fi
[ -f "${libpath}" ] || fail "expected a library at ${libpath}"
echo "run-examples: library $(wc -c < "${libpath}" | tr -d ' ') bytes"

# --- the gateway binary ------------------------------------------------------
bin="${here}/bin/llmux"
if [ ! -x "${bin}" ]; then
  echo "run-examples: building the gateway binary…"
  mkdir -p "${here}/bin"
  ( cd "${root}" && go build -o "${bin}" ./cmd/llmux )
fi

# --- the fake upstream -------------------------------------------------------
( cd "${root}/ffi" && go build -o "${tmp}/fakeupstream" ./fakeupstream )
"${tmp}/fakeupstream" -text "alpha bravo charlie" >"${tmp}/fake.out" 2>&1 &
fake_pid=$!
config=""
for _ in $(seq 1 200); do
  if grep -q '^CONFIG ' "${tmp}/fake.out" 2>/dev/null; then
    config="$(sed -n 's/^CONFIG //p' "${tmp}/fake.out" | head -1)"
    break
  fi
  kill -0 "${fake_pid}" 2>/dev/null || { cat "${tmp}/fake.out" >&2; fail "the fake upstream died"; }
  sleep 0.05
done
[ -n "${config}" ] || fail "the fake upstream never announced a CONFIG"
printf '%s' "${config}" > "${tmp}/llmux.json"
echo "run-examples: upstream $(sed -n 's/^URL //p' "${tmp}/fake.out" | head -1)"

# --- build -------------------------------------------------------------------
dotnet build "${here}/examples/Examples.csproj" -v q -c Release -o "${tmp}/out" \
  >"${tmp}/dotnet.log" 2>&1 || { cat "${tmp}/dotnet.log" >&2; fail "the examples did not build"; }
echo "run-examples: built"

# --- run ---------------------------------------------------------------------
status=0
run() {
  LLMUX_LIBRARY="${libpath}" \
  LLMUX_CONFIG_JSON="${config}" \
  LLMUX_BINARY="${bin}" \
  LLMUX_CONFIG="${tmp}/llmux.json" \
    dotnet "${tmp}/out/llmux-examples.dll" "$1" || status=1
}

echo
run "${which}"

echo
[ "${status}" -eq 0 ] || fail "an example exited non-zero"
echo "run-examples: OK"

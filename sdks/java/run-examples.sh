#!/usr/bin/env bash
#
# run-examples.sh — compile and RUN both Java examples, direct and sidecar.
#
# It stands up everything they need:
#   * the shared library     (scripts/build-ffi.sh)      for DirectChat
#   * the gateway binary     (go build ./cmd/llmux)      for SidecarChat
#   * a fake OpenAI upstream (ffi/fakeupstream)          for both, so neither
#     example needs an API key or a network
#
# It fails closed. A missing JDK, a JDK older than 22, a library that would not
# build, or an example that exits non-zero is a FAILURE, never a silent skip —
# an example nobody ran is not an example.
#
# Usage:  sdks/java/run-examples.sh [direct|sidecar]
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
which="${1:-both}"

fail() { echo "run-examples: FAIL — $*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || fail "go is not on PATH"
command -v javac >/dev/null 2>&1 || fail "javac is not on PATH"
command -v java >/dev/null 2>&1 || fail "java is not on PATH"

# --- the JDK must be new enough for java.lang.foreign -----------------------
jdk_major="$(java -XshowSettings:properties -version 2>&1 \
  | sed -n 's/^ *java\.specification\.version *= *//p' | cut -d. -f1)"
[ -n "${jdk_major}" ] || fail "could not determine the java version"
if [ "${jdk_major}" -lt 22 ]; then
  fail "Java ${jdk_major} is too old for the direct example. java.lang.foreign
       became a permanent API in Java 22. The SIDECAR example runs on Java 11+;
       run it with: sdks/java/run-examples.sh sidecar"
fi
echo "run-examples: JDK ${jdk_major} ($(java -version 2>&1 | head -1))"

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

# --- 1. the shared library ---------------------------------------------------
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
echo "run-examples: library $(wc -c < "${libpath}" | tr -d ' ') bytes at ${libpath}"

# --- 2. the gateway binary (for the sidecar example) ------------------------
bin="${here}/bin/llmux"
if [ ! -x "${bin}" ]; then
  echo "run-examples: building the gateway binary…"
  mkdir -p "${here}/bin"
  ( cd "${root}" && go build -o "${bin}" ./cmd/llmux )
fi

# --- 3. the fake upstream ----------------------------------------------------
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
echo "run-examples: upstream $(sed -n 's/^URL //p' "${tmp}/fake.out" | head -1)"

printf '%s' "${config}" > "${tmp}/llmux.json"

# --- 4. compile --------------------------------------------------------------
out="${tmp}/classes"
mkdir -p "${out}"
javac -d "${out}" "${here}"/src/main/java/to/llmux/*.java
javac -d "${out}" -cp "${out}" "${here}"/examples/*.java
echo "run-examples: compiled"

# --- 5. run ------------------------------------------------------------------
status=0

if [ "${which}" = "both" ] || [ "${which}" = "direct" ]; then
  echo
  echo "================ DirectChat (in-process, C ABI) ================"
  LLMUX_LIBRARY="${libpath}" LLMUX_CONFIG_JSON="${config}" \
    java --enable-native-access=ALL-UNNAMED -cp "${out}" DirectChat || status=1
fi

if [ "${which}" = "both" ] || [ "${which}" = "sidecar" ]; then
  echo
  echo "================ SidecarChat (child process, HTTP) ============="
  LLMUX_BINARY="${bin}" LLMUX_CONFIG="${tmp}/llmux.json" \
    java -cp "${out}" SidecarChat || status=1
fi

echo
[ "${status}" -eq 0 ] || fail "an example exited non-zero"
echo "run-examples: OK"

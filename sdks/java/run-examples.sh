#!/usr/bin/env bash
#
# run-examples.sh — compile and RUN the Java examples: direct, sidecar, and the
# llmux_cancel demonstration.
#
# It stands up everything they need:
#   * the shared library     (scripts/build-ffi.sh)      for DirectChat/CancelChat
#   * the gateway binary     (go build ./cmd/llmux)      for SidecarChat
#   * a fake OpenAI upstream                             for all three, so none
#     of them needs an API key or a network
#
# `direct` and `sidecar` point at ffi/fakeupstream (Go, no delay, no counter —
# fine for a completion that only needs to exist). `cancel` points at
# sdks/fake-upstream.py instead: cancelling mid-stream needs a chunk delay
# worth cancelling INTO, and a GET /generated to prove the provider actually
# stopped rather than merely going unheard. ffi/fakeupstream has neither.
#
# It fails closed. A missing JDK, a JDK older than 22, a library that would not
# build, or an example that exits non-zero is a FAILURE, never a silent skip —
# an example nobody ran is not an example.
#
# Usage:  sdks/java/run-examples.sh [direct|sidecar|cancel|both]
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
  fail "Java ${jdk_major} is too old for the direct/cancel examples. java.lang.foreign
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

# --- the shared library, needed by every mode except a plain sidecar run ----
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

# ------------------------------------------------------------------------- #
# cancel: llmux_cancel against sdks/fake-upstream.py, its own mode because it
# needs a different upstream than direct/sidecar do.
# ------------------------------------------------------------------------- #
if [ "${which}" = "cancel" ]; then
  command -v python3 >/dev/null 2>&1 \
    || fail "python3 is not on PATH — sdks/fake-upstream.py needs it"
  fake_upstream="${root}/sdks/fake-upstream.py"
  [ -f "${fake_upstream}" ] || fail "expected ${fake_upstream}"

  # 100 ms/chunk on a 10-word answer: long enough to reliably cancel after the
  # 3rd chunk from a second JVM thread without the whole stream having already
  # finished underneath it.
  python3 "${fake_upstream}" --chunk-delay-ms 100 \
    --text "one two three four five six seven eight nine ten" \
    >"${tmp}/fake.out" 2>&1 &
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

  out="${tmp}/classes"
  mkdir -p "${out}"
  javac -d "${out}" "${here}"/src/main/java/to/llmux/*.java
  javac -d "${out}" -cp "${out}" "${here}"/examples/CancelChat.java
  echo "run-examples: compiled"

  echo
  echo "================ CancelChat (llmux_cancel, sdks/fake-upstream.py) ================"
  status=0
  LLMUX_LIBRARY="${libpath}" LLMUX_CONFIG_JSON="${config}" \
    java --enable-native-access=ALL-UNNAMED -cp "${out}" CancelChat || status=1

  echo
  [ "${status}" -eq 0 ] || fail "CancelChat exited non-zero"
  echo "run-examples: OK"
  exit 0
fi

# ------------------------------------------------------------------------- #
# direct / sidecar / both, against ffi/fakeupstream as before.
# ------------------------------------------------------------------------- #

# --- the gateway binary (for the sidecar example) ------------------------
bin="${here}/bin/llmux"
if [ ! -x "${bin}" ]; then
  echo "run-examples: building the gateway binary…"
  mkdir -p "${here}/bin"
  ( cd "${root}" && go build -o "${bin}" ./cmd/llmux )
fi

# --- the fake upstream ----------------------------------------------------
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

# --- compile --------------------------------------------------------------
out="${tmp}/classes"
mkdir -p "${out}"
javac -d "${out}" "${here}"/src/main/java/to/llmux/*.java
javac -d "${out}" -cp "${out}" "${here}"/examples/*.java
echo "run-examples: compiled"

# --- run --------------------------------------------------------------------
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

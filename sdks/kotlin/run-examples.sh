#!/usr/bin/env bash
#
# run-examples.sh — compile and RUN the Kotlin examples: direct, sidecar, and
# the llmux_cancel demonstration.
#
# The Kotlin SDK is a wrapper over the Java one, so this compiles the Java
# sources first with javac and puts them on kotlinc's classpath. It also stands
# up everything the examples need: the shared library, the gateway binary, and
# a fake OpenAI upstream, so none of them needs an API key or a network.
#
# `direct` and `sidecar` point at ffi/fakeupstream (Go, no delay, no counter).
# `cancel` points at sdks/fake-upstream.py instead: cancelling mid-stream needs
# a chunk delay worth cancelling INTO, and a GET /generated to prove the
# provider actually stopped rather than merely going unheard. ffi/fakeupstream
# has neither.
#
# The one third-party dependency is kotlinx-coroutines-core (Flow). It is
# resolved from the local Maven repository, and fetched with `mvn
# dependency:get` if it is not there. No Gradle: see README.md for why this
# repo drives kotlinc directly.
#
# Fails closed. A missing toolchain or a non-zero example is a FAILURE.
#
# Usage:  sdks/kotlin/run-examples.sh [direct|sidecar|cancel|both]
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"
java_sdk="${root}/sdks/java"
which="${1:-both}"

COROUTINES_VERSION="1.10.2"

fail() { echo "run-examples: FAIL — $*" >&2; exit 1; }

for tool in go javac java kotlinc; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is not on PATH"
done

jdk_major="$(java -XshowSettings:properties -version 2>&1 \
  | sed -n 's/^ *java\.specification\.version *= *//p' | cut -d. -f1)"
[ -n "${jdk_major}" ] || fail "could not determine the java version"
if [ "${jdk_major}" -lt 22 ]; then
  fail "Java ${jdk_major} is too old. The Kotlin SDK compiles against
       to.llmux.LlmuxDirect, which is a Java 22 class file. The SIDECAR half
       needs only Java 11 — but this script builds both."
fi
echo "run-examples: JDK ${jdk_major}, $(kotlinc -version 2>&1 | head -1)"

# --- the coroutines jar ------------------------------------------------------
m2="${HOME}/.m2/repository"
coroutines="${m2}/org/jetbrains/kotlinx/kotlinx-coroutines-core-jvm/${COROUTINES_VERSION}/kotlinx-coroutines-core-jvm-${COROUTINES_VERSION}.jar"
if [ ! -f "${coroutines}" ]; then
  command -v mvn >/dev/null 2>&1 \
    || fail "kotlinx-coroutines-core-jvm ${COROUTINES_VERSION} is not in ${m2} and mvn is not on PATH to fetch it"
  echo "run-examples: fetching kotlinx-coroutines-core-jvm ${COROUTINES_VERSION}…"
  mvn -B -q dependency:get \
    "-Dartifact=org.jetbrains.kotlinx:kotlinx-coroutines-core-jvm:${COROUTINES_VERSION}" \
    || fail "could not fetch kotlinx-coroutines-core-jvm"
fi
[ -f "${coroutines}" ] || fail "expected the coroutines jar at ${coroutines}"

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

# --- the shared library, needed by every mode --------------------------------
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

# kotlin-stdlib.jar has to be on the RUN classpath; kotlinc only puts it on the
# compile one. Finding it means resolving through however kotlinc was installed
# — a Homebrew symlink, an SDKMAN shim, or a plain unpacked distribution — so
# each candidate is checked and the failure names every path tried.
find_stdlib() {
  local candidates=() c bin real
  [ -n "${KOTLIN_HOME:-}" ] && candidates+=("${KOTLIN_HOME}/lib/kotlin-stdlib.jar")
  bin="$(command -v kotlinc)"
  real="${bin}"
  # Follow the symlink chain by hand: `readlink -f` is GNU and is absent on
  # some macOS versions, and a missing tool here would look like a missing jar.
  while [ -L "${real}" ]; do
    local target; target="$(readlink "${real}")"
    case "${target}" in
      /*) real="${target}" ;;
      *)  real="$(dirname "${real}")/${target}" ;;
    esac
  done
  for c in "$(dirname "$(dirname "${real}")")" "$(dirname "$(dirname "${bin}")")"; do
    candidates+=("${c}/lib/kotlin-stdlib.jar" "${c}/libexec/lib/kotlin-stdlib.jar")
  done
  if command -v brew >/dev/null 2>&1; then
    candidates+=("$(brew --prefix kotlin 2>/dev/null)/libexec/lib/kotlin-stdlib.jar")
  fi
  for c in "${candidates[@]}"; do
    if [ -f "${c}" ]; then echo "${c}"; return 0; fi
  done
  printf 'run-examples: FAIL — could not find kotlin-stdlib.jar. Tried:\n' >&2
  printf '  %s\n' "${candidates[@]}" >&2
  printf 'Set KOTLIN_HOME to the Kotlin distribution root.\n' >&2
  exit 1
}

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
  # 3rd chunk — from a second coroutine, or a withTimeout deadline — without
  # the whole stream having already finished underneath it.
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

  classes="${tmp}/classes"
  mkdir -p "${classes}"
  javac -d "${classes}" "${java_sdk}"/src/main/java/to/llmux/*.java
  kotlinc -nowarn -jvm-target 22 \
    -classpath "${classes}:${coroutines}" \
    -d "${classes}" \
    "${here}"/src/main/kotlin/to/llmux/kotlin/*.kt 2>&1 | grep -v '^warning:' || true
  kotlinc -nowarn -jvm-target 22 \
    -classpath "${classes}:${coroutines}" \
    -d "${classes}" \
    "${here}"/examples/CancelChat.kt 2>&1 | grep -v '^warning:' || true
  [ -f "${classes}/CancelChatKt.class" ] || fail "CancelChat.kt did not compile"
  echo "run-examples: compiled"

  stdlib="$(find_stdlib)"
  echo
  echo "================ CancelChat (llmux_cancel, sdks/fake-upstream.py) ================"
  status=0
  LLMUX_LIBRARY="${libpath}" LLMUX_CONFIG_JSON="${config}" \
    java --enable-native-access=ALL-UNNAMED -cp "${classes}:${coroutines}:${stdlib}" CancelChatKt \
    || status=1

  echo
  [ "${status}" -eq 0 ] || fail "CancelChat exited non-zero"
  echo "run-examples: OK"
  exit 0
fi

# ------------------------------------------------------------------------- #
# direct / sidecar / both, against ffi/fakeupstream as before.
# ------------------------------------------------------------------------- #

# --- the gateway binary ------------------------------------------------------
bin="${java_sdk}/bin/llmux"
if [ ! -x "${bin}" ]; then
  echo "run-examples: building the gateway binary…"
  mkdir -p "${java_sdk}/bin"
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

# --- compile -----------------------------------------------------------------
classes="${tmp}/classes"
mkdir -p "${classes}"

# 1. the Java SDK (LlmuxDirect is a Java 22 class file; the rest is 11)
javac -d "${classes}" "${java_sdk}"/src/main/java/to/llmux/*.java

# 2. the Kotlin SDK, against those classes
kotlinc -nowarn -jvm-target 22 \
  -classpath "${classes}:${coroutines}" \
  -d "${classes}" \
  "${here}"/src/main/kotlin/to/llmux/kotlin/*.kt 2>&1 | grep -v '^warning:' || true

# 3. the examples (direct/sidecar only; CancelChat.kt needs the python harness
#    and is compiled separately above)
kotlinc -nowarn -jvm-target 22 \
  -classpath "${classes}:${coroutines}" \
  -d "${classes}" \
  "${here}"/examples/DirectChat.kt "${here}"/examples/SidecarChat.kt 2>&1 | grep -v '^warning:' || true

[ -f "${classes}/DirectChatKt.class" ] || fail "DirectChat.kt did not compile"
[ -f "${classes}/SidecarChatKt.class" ] || fail "SidecarChat.kt did not compile"
echo "run-examples: compiled"

stdlib="$(find_stdlib)"
cp_run="${classes}:${coroutines}:${stdlib}"
status=0

if [ "${which}" = "both" ] || [ "${which}" = "direct" ]; then
  echo
  echo "================ DirectChat (in-process, C ABI) ================"
  LLMUX_LIBRARY="${libpath}" LLMUX_CONFIG_JSON="${config}" \
    java --enable-native-access=ALL-UNNAMED -cp "${cp_run}" DirectChatKt || status=1
fi

if [ "${which}" = "both" ] || [ "${which}" = "sidecar" ]; then
  echo
  echo "================ SidecarChat (child process, HTTP) ============="
  LLMUX_BINARY="${bin}" LLMUX_CONFIG="${tmp}/llmux.json" \
    java -cp "${cp_run}" SidecarChatKt || status=1
fi

echo
[ "${status}" -eq 0 ] || fail "an example exited non-zero"
echo "run-examples: OK"

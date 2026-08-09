#!/usr/bin/env bash
#
# signal-probe.sh — measure what loading libllmux does to this JVM's signal
# handlers, and whether the JVM still works afterwards.
#
# README.md's "The JVM and Go's signal handlers" section is the output of this
# script, not a recollection. Re-run it on your JDK and your platform; the
# answer is allowed to be different from ours, which is the whole reason it is
# a script and not a paragraph.
#
# Usage:
#   sdks/java/signal-probe.sh            # plain
#   sdks/java/signal-probe.sh --jsig     # with libjsig preloaded (the fix)
#   sdks/java/signal-probe.sh --checkjni # with HotSpot's own handler audit on
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/../.." && pwd)"

fail() { echo "signal-probe: FAIL — $*" >&2; exit 1; }

command -v java >/dev/null 2>&1 || fail "java is not on PATH"
command -v javac >/dev/null 2>&1 || fail "javac is not on PATH"

jdk_major="$(java -XshowSettings:properties -version 2>&1 \
  | sed -n 's/^ *java\.specification\.version *= *//p' | cut -d. -f1)"
[ -n "${jdk_major}" ] && [ "${jdk_major}" -ge 22 ] \
  || fail "Java 22+ is required (java.lang.foreign); this is Java ${jdk_major:-?}"

use_jsig=0
checkjni=0
for arg in "$@"; do
  case "${arg}" in
    --jsig) use_jsig=1 ;;
    --checkjni) checkjni=1 ;;
    *) fail "unknown argument: ${arg}" ;;
  esac
done

goos="$(go env GOOS 2>/dev/null || echo unknown)"
goarch="$(go env GOARCH 2>/dev/null || echo unknown)"
case "${goos}" in
  darwin) libfile="libllmux.dylib" ;;
  windows) libfile="llmux.dll" ;;
  *) libfile="libllmux.so" ;;
esac
libpath="${LLMUX_LIBRARY:-${root}/dist/ffi/${goos}_${goarch}/${libfile}}"
if [ ! -f "${libpath}" ]; then
  echo "signal-probe: building ${libfile}…"
  "${root}/scripts/build-ffi.sh" >/dev/null 2>&1 || fail "the shared library did not build"
fi
[ -f "${libpath}" ] || fail "no library at ${libpath}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
javac -d "${tmp}" "${here}/tools/SignalHandlerProbe.java"

jvm_args=(--enable-native-access=ALL-UNNAMED)
[ "${checkjni}" -eq 1 ] && jvm_args+=(-Xcheck:jni)

if [ "${use_jsig}" -eq 1 ]; then
  java_home="$(java -XshowSettings:properties -version 2>&1 \
    | sed -n 's/^ *java\.home *= *//p')"
  case "${goos}" in
    darwin) jsig="${java_home}/lib/libjsig.dylib" ;;
    *)      jsig="${java_home}/lib/libjsig.so" ;;
  esac
  [ -f "${jsig}" ] || fail "no libjsig at ${jsig} — this JDK does not ship it"
  echo "signal-probe: preloading ${jsig}"
  if [ "${goos}" = "darwin" ]; then
    DYLD_INSERT_LIBRARIES="${jsig}" java "${jvm_args[@]}" -cp "${tmp}" \
      SignalHandlerProbe "${libpath}" llmux_abi_version
  else
    LD_PRELOAD="${jsig}" java "${jvm_args[@]}" -cp "${tmp}" \
      SignalHandlerProbe "${libpath}" llmux_abi_version
  fi
else
  java "${jvm_args[@]}" -cp "${tmp}" SignalHandlerProbe "${libpath}" llmux_abi_version
fi

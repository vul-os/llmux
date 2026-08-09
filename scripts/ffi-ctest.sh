#!/usr/bin/env bash
#
# ffi-ctest.sh — build libllmux and run the C smoke test against it.
#
# This is the step that makes the C ABI verified rather than asserted. Every Go
# test in ffi/ would pass just as happily with a missing //export directive, a
# renamed symbol, or a header that no longer matches the library. Only a program
# that dlopens the artifact and calls it through the header can catch that.
#
# What it does:
#   1. builds the shared library for the host (scripts/build-ffi.sh),
#   2. starts ffi/fakeupstream, a fake OpenAI-compatible server which PRINTS the
#      llmux config JSON pointing at itself (so the C test does not hand-roll a
#      second copy of the configuration schema),
#   3. compiles ffi/ctest/smoke.c against ffi/include/llmux.h,
#   4. runs it with the library path, the version from ./VERSION, that config,
#      and the expected answer text.
#
# It fails closed: no compiler, no library, no upstream, or a smoke test that
# ran the wrong number of checks is a FAILURE, never a skip.
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/.." && pwd)"
ffi_dir="${root}/ffi"

# Distinct enough that a truncated stream or an off-by-one in the chunk
# reassembly is visible in the diff rather than plausible.
TEXT="alpha bravo charlie delta echo foxtrot"

for tool in go cc; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "ffi-ctest: FAIL — ${tool} is not on PATH. The C ABI cannot be verified without it; " \
         "this is a failure, not a skip." >&2
    exit 1
  fi
done

version="$(tr -d '[:space:]' < "${root}/VERSION")"
if [ -z "${version}" ]; then
  echo "ffi-ctest: FAIL — ./VERSION is empty, so the abi-version check would compare against nothing" >&2
  exit 1
fi

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

# --- 1. the library ----------------------------------------------------------
"${here}/build-ffi.sh" --out "${tmp}/dist" >"${tmp}/build.log" 2>&1 || {
  echo "ffi-ctest: FAIL — the shared library did not build:" >&2
  cat "${tmp}/build.log" >&2
  exit 1
}
host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
case "${host_os}" in
  windows) libfile="llmux.dll" ;;
  darwin)  libfile="libllmux.dylib" ;;
  *)       libfile="libllmux.so" ;;
esac
libpath="${tmp}/dist/${host_os}_${host_arch}/${libfile}"
if [ ! -f "${libpath}" ]; then
  echo "ffi-ctest: FAIL — expected a library at ${libpath} and there is none:" >&2
  cat "${tmp}/build.log" >&2
  exit 1
fi
echo "ffi-ctest: library $(wc -c < "${libpath}" | tr -d ' ') bytes at ${libpath}"

# --- 2. the fake upstream ----------------------------------------------------
( cd "${ffi_dir}" && go build -o "${tmp}/fakeupstream" ./fakeupstream )
"${tmp}/fakeupstream" -text "${TEXT}" >"${tmp}/fake.out" 2>&1 &
fake_pid=$!

# Wait for its three announcement lines. There is no `timeout` command on macOS,
# so poll with a bounded loop rather than pretending one exists.
config=""
for _ in $(seq 1 200); do
  if grep -q '^CONFIG ' "${tmp}/fake.out" 2>/dev/null; then
    config="$(sed -n 's/^CONFIG //p' "${tmp}/fake.out" | head -1)"
    break
  fi
  if ! kill -0 "${fake_pid}" 2>/dev/null; then
    echo "ffi-ctest: FAIL — the fake upstream exited before announcing itself:" >&2
    cat "${tmp}/fake.out" >&2
    exit 1
  fi
  sleep 0.05
done
if [ -z "${config}" ]; then
  echo "ffi-ctest: FAIL — the fake upstream never printed a CONFIG line:" >&2
  cat "${tmp}/fake.out" >&2
  exit 1
fi
echo "ffi-ctest: upstream $(sed -n 's/^URL //p' "${tmp}/fake.out" | head -1)"

# --- 3. compile the C test ---------------------------------------------------
ldflags=(-lpthread)
if [ "${host_os}" != "darwin" ]; then
  ldflags+=(-ldl)   # macOS has dlopen in libc; glibc needs -ldl on older toolchains
fi
cc -std=c11 -Wall -Wextra -Werror -O1 \
   -I "${ffi_dir}/include" \
   -o "${tmp}/smoke" "${ffi_dir}/ctest/smoke.c" "${ldflags[@]}"

# --- 4. run it ---------------------------------------------------------------
"${tmp}/smoke" "${libpath}" "${version}" "${config}" "${TEXT}"

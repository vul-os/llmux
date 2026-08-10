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
# With --selftest it then does step 5: rebuilds the library with the version
# derivation deliberately broken and requires the SAME smoke binary to reject
# it. See the comment on that step for why the version probe in particular needs
# that treatment.
#
# It fails closed: no compiler, no library, no upstream, or a smoke test that
# ran the wrong number of checks is a FAILURE, never a skip.
#
set -euo pipefail

selftest=0
while [ $# -gt 0 ]; do
  case "$1" in
    --selftest) selftest=1; shift ;;
    -h|--help) sed -n '2,26p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "ffi-ctest: unknown argument $1" >&2; exit 2 ;;
  esac
done

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

if [ "${selftest}" -eq 0 ]; then
  echo "ffi-ctest: OK — run with --selftest to also prove the version probe can fail."
  exit 0
fi

# --- 5. selftest: the version derivation must be load-bearing ----------------
#
# llmux_abi_version used to report a hand-typed constant in ffi/abi.go, and in
# v0.1.3 that constant lagged the release: the library told every host it was
# 0.1.2 when it was 0.1.3, so the staleness check the symbol exists to serve
# answered "stale" about a current library. It is now derived — ffi/abi.go's
# Version is llmux.Version, which is //go:embed'ed from ./VERSION one module up
# and reached through the `replace` directive in ffi/go.mod.
#
# That is three links (embed, TrimSpace, replace) and none of them is visible in
# the built artifact. If any of them silently stopped connecting, every test
# above would still pass: they would all be reading the same wrong-but-consistent
# value, which is exactly how the original defect went out green.
#
# So: rebuild the library with the LIBRARY's version string mutated, one module
# away from the ABI, and require the smoke binary — the same one, built against
# the same header, comparing against the same ./VERSION — to notice. If it does
# not, the derivation is decorative and the string is coming from somewhere else.
#
# `go build -overlay` swaps the file for the build only; nothing in the checkout
# is touched.

echo
echo "ffi-ctest: selftest — breaking the version derivation, the smoke test must catch it"

mut="${tmp}/mut"
mkdir -p "${mut}"
sed 's|^var Version = strings\.TrimSpace(versionFile)$|var Version = strings.TrimSpace(versionFile) + "-mutant"|' \
  "${root}/version.go" > "${mut}/version.go"
if cmp -s "${root}/version.go" "${mut}/version.go"; then
  echo "ffi-ctest: FAIL — the mutation changed nothing in version.go. The sed expression no longer" \
       "matches, so this case would test an unmodified library and report a green it did not earn." >&2
  exit 1
fi
printf '{"Replace":{"%s":"%s"}}\n' "${root}/version.go" "${mut}/version.go" > "${mut}/overlay.json"

mutlib="${mut}/${libfile}"
if ! ( cd "${ffi_dir}" && CGO_ENABLED=1 go build -overlay "${mut}/overlay.json" \
         -buildmode=c-shared -o "${mutlib}" . ) >"${mut}/build.log" 2>&1; then
  echo "ffi-ctest: FAIL — the mutated library did not build; a mutation that cannot be compiled" \
       "proves nothing:" >&2
  cat "${mut}/build.log" >&2
  exit 1
fi

if "${tmp}/smoke" "${mutlib}" "${version}" "${config}" "${TEXT}" >"${mut}/smoke.log" 2>&1; then
  echo "ffi-ctest: FAIL — the smoke test PASSED against a library whose version string was" \
       "deliberately corrupted. The version probe is not checking what it claims to." >&2
  sed 's/^/    /' "${mut}/smoke.log" >&2
  exit 1
fi
echo "ffi-ctest: caught — a library built from a mutated ./VERSION is rejected"
sed -n 's/^  FAIL/    FAIL/p' "${mut}/smoke.log" | head -3
echo "ffi-ctest: OK — the version probe is capable of failing."

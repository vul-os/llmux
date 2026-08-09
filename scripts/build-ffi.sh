#!/usr/bin/env bash
#
# build-ffi.sh — build the C-ABI shared library from ffi/.
#
# `go build -buildmode=c-shared` needs cgo, and cgo needs a C toolchain FOR THE
# TARGET. That is the whole difficulty of this script: a Go cross-compile is one
# environment variable, a cgo cross-compile is a cross C compiler you either
# have or do not. So this script builds the host target, attempts the others
# only when it can actually find a toolchain for them, and PRINTS WHAT IT DID —
# including, by name, every target it skipped and why.
#
# It never pretends. A summary line that says "built: darwin/arm64; skipped:
# linux/amd64 (no cross C toolchain)" is the point of the exercise; a release
# note claiming three platforms because a loop ran three times is not.
#
# Usage:
#   scripts/build-ffi.sh                 # host target only
#   scripts/build-ffi.sh --all           # host + every target with a toolchain
#   scripts/build-ffi.sh --out DIR       # output root (default dist/ffi)
#
# Output layout:
#   <out>/<goos>_<goarch>/libllmux.{so,dylib} | llmux.dll
#   <out>/<goos>_<goarch>/libllmux.h     (cgo-generated header)
#   <out>/include/llmux.h                (the stable hand-written header)
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/.." && pwd)"
ffi_dir="${root}/ffi"
out_root="${root}/dist/ffi"
want_all=0

while [ $# -gt 0 ]; do
  case "$1" in
    --all) want_all=1; shift ;;
    --out) out_root="${2:?--out needs a directory}"; shift 2 ;;
    -h|--help) sed -n '2,30p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "build-ffi: unknown argument $1" >&2; exit 2 ;;
  esac
done

if [ ! -f "${ffi_dir}/go.mod" ]; then
  echo "build-ffi: FAIL — no ffi/go.mod under ${root}; nothing to build" >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  echo "build-ffi: FAIL — no go toolchain on PATH" >&2
  exit 1
fi

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"

built=()
skipped=()

# lib_name GOOS -> the conventional shared-library filename for that platform.
lib_name() {
  case "$1" in
    windows) echo "llmux.dll" ;;
    darwin)  echo "libllmux.dylib" ;;
    *)       echo "libllmux.so" ;;
  esac
}

# find_cc GOOS GOARCH -> prints a C compiler command able to target it, or
# nothing. The host's own cc is only offered for the host target: a native
# clang cannot produce a Linux ELF, and pretending it can is how a build script
# starts lying.
find_cc() {
  local goos="$1" goarch="$2"
  if [ "${goos}" = "${host_os}" ] && [ "${goarch}" = "${host_arch}" ]; then
    if command -v cc >/dev/null 2>&1; then echo "cc"; return; fi
    if command -v gcc >/dev/null 2>&1; then echo "gcc"; return; fi
    if command -v clang >/dev/null 2>&1; then echo "clang"; return; fi
    return
  fi
  case "${goos}/${goarch}" in
    linux/amd64)
      for c in x86_64-linux-gnu-gcc x86_64-unknown-linux-gnu-gcc musl-gcc; do
        if command -v "$c" >/dev/null 2>&1; then echo "$c"; return; fi
      done
      if command -v zig >/dev/null 2>&1; then echo "zig cc -target x86_64-linux-gnu"; return; fi
      ;;
    linux/arm64)
      for c in aarch64-linux-gnu-gcc aarch64-unknown-linux-gnu-gcc; do
        if command -v "$c" >/dev/null 2>&1; then echo "$c"; return; fi
      done
      if command -v zig >/dev/null 2>&1; then echo "zig cc -target aarch64-linux-gnu"; return; fi
      ;;
    windows/amd64)
      if command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then echo "x86_64-w64-mingw32-gcc"; return; fi
      if command -v zig >/dev/null 2>&1; then echo "zig cc -target x86_64-windows-gnu"; return; fi
      ;;
    darwin/amd64|darwin/arm64)
      # Cross-building a macOS dylib needs the macOS SDK. Only offer the host
      # compiler when we are already on macOS and the arch is one clang here
      # can target.
      if [ "${host_os}" = "darwin" ] && command -v cc >/dev/null 2>&1; then echo "cc"; return; fi
      ;;
  esac
}

build_target() {
  local goos="$1" goarch="$2"
  local cc; cc="$(find_cc "${goos}" "${goarch}")"
  if [ -z "${cc}" ]; then
    skipped+=("${goos}/${goarch} — no C toolchain on this machine that can target it")
    return 0
  fi
  local dir="${out_root}/${goos}_${goarch}"
  local lib; lib="$(lib_name "${goos}")"
  mkdir -p "${dir}"
  echo ">> ${goos}/${goarch}  (CC=${cc})"
  if ! (
    cd "${ffi_dir}" &&
    CGO_ENABLED=1 GOOS="${goos}" GOARCH="${goarch}" CC="${cc}" \
      go build -buildmode=c-shared -trimpath -o "${dir}/${lib}" .
  ); then
    skipped+=("${goos}/${goarch} — the build FAILED (see the output above)")
    return 0
  fi
  # `go build -buildmode=c-shared` stamps a BARE install name on a dylib
  # ("libllmux.dylib", no prefix). dyld only consults an executable's -rpath
  # entries for dependencies recorded as "@rpath/...", so a consumer that LINKS
  # against this library — rather than dlopen'ing it by path — dies at startup
  # with `Library not loaded: libllmux.dylib` no matter how many -Wl,-rpath
  # flags it passed. dlopen callers never notice, which is why this survived:
  # the C smoke test and every example here dlopen by absolute path.
  if [ "${goos}" = "darwin" ] && command -v install_name_tool >/dev/null 2>&1; then
    install_name_tool -id "@rpath/${lib}" "${dir}/${lib}" || {
      skipped+=("${goos}/${goarch} — built, but install_name_tool failed; linking consumers will break")
      return 0
    }
  fi
  local size; size="$(wc -c < "${dir}/${lib}" | tr -d ' ')"
  built+=("${goos}/${goarch}  ${dir}/${lib}  ${size} bytes")
}

mkdir -p "${out_root}/include"
cp "${ffi_dir}/include/llmux.h" "${out_root}/include/llmux.h"

build_target "${host_os}" "${host_arch}"
if [ "${want_all}" = "1" ]; then
  for t in linux/amd64 linux/arm64 windows/amd64; do
    goos="${t%%/*}"; goarch="${t##*/}"
    if [ "${goos}" = "${host_os}" ] && [ "${goarch}" = "${host_arch}" ]; then
      continue
    fi
    build_target "${goos}" "${goarch}"
  done
fi

echo
echo "build-ffi: stable header -> ${out_root}/include/llmux.h"
if [ "${#built[@]}" -eq 0 ]; then
  echo "build-ffi: FAIL — nothing was built." >&2
  printf '  skipped: %s\n' "${skipped[@]}" >&2
  exit 1
fi
echo "build-ffi: BUILT"
printf '  %s\n' "${built[@]}"
if [ "${#skipped[@]}" -gt 0 ]; then
  echo "build-ffi: NOT BUILT on this machine"
  printf '  %s\n' "${skipped[@]}"
fi

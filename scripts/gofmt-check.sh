#!/usr/bin/env bash
#
# gofmt gate — covers EVERY directory that holds first-party Go source.
#
# Why this exists as a script instead of a one-line CI step: the previous gate
# was `gofmt -l core cmd sdks web site`, a hand-written directory list. That
# list silently excluded `integration/` (the control-plane seam), so an
# unformatted file there sailed through CI. A hand-maintained scope is a gate
# that goes blind the day someone adds a top-level directory and forgets it.
#
# This script DISCOVERS the scope instead of hardcoding it, and refuses to run
# if the discovery looks broken:
#
#   * It fails closed. There is no skip path: no gofmt, no repo root, or a
#     discovery that finds implausibly little source is a FAILURE, not a pass.
#   * It asserts a coverage floor (file + directory counts) before checking
#     anything, so a broken `find`, a bad root, or an over-eager exclusion
#     cannot make this gate "pass" by inspecting nothing.
#   * It prints the exact scope it verified, so a green run is readable
#     evidence rather than silence.
#
set -euo pipefail

# --- locate the repo root (the module root), fail loudly if we cannot ---------
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/.." && pwd)"
if [ ! -f "${root}/go.mod" ]; then
  echo "gofmt gate: FAIL — no go.mod at ${root}; the gate located no module and verified NOTHING" >&2
  exit 1
fi
cd "${root}"

if ! command -v gofmt >/dev/null 2>&1; then
  echo "gofmt gate: FAIL — gofmt is not installed; formatting of NO Go file was verified" >&2
  exit 1
fi

# --- coverage floors ---------------------------------------------------------
# Deliberately set below the current counts (144 files / 23 dirs at the time of
# writing) so ordinary growth does not trip them, but high enough that a
# discovery bug — wrong root, exclusion that eats the tree, find that matches
# nothing — cannot slip past as a green run.
MIN_GO_FILES=120
MIN_GO_DIRS=20

# --- discover the scope ------------------------------------------------------
# Every .go file in the repo, minus vendored JS and git internals. Nothing else
# is excluded: `integration/`, `ee/`, and any directory added tomorrow are in
# scope automatically.
# (Portable read loop rather than `mapfile`, so this runs on the stock macOS
# bash 3.2 as well as CI's bash 5.)
go_files=()
while IFS= read -r f; do
  go_files+=("$f")
done < <(
  find . \
    \( -path ./.git -o -path ./web/node_modules -o -path ./web/dist -o -path ./dist \) -prune -o \
    -type f -name '*.go' -print | sort
)

file_count=${#go_files[@]}
if [ "${file_count}" -lt "${MIN_GO_FILES}" ]; then
  echo "gofmt gate: FAIL — discovered only ${file_count} Go files (floor ${MIN_GO_FILES})." >&2
  echo "  The scan is broken, not the repo. NOTHING was verified; fix discovery before trusting this gate." >&2
  exit 1
fi

go_dirs=()
while IFS= read -r d; do
  go_dirs+=("$d")
done < <(printf '%s\n' "${go_files[@]}" | xargs -n1 dirname | sort -u)
dir_count=${#go_dirs[@]}
if [ "${dir_count}" -lt "${MIN_GO_DIRS}" ]; then
  echo "gofmt gate: FAIL — discovered only ${dir_count} Go directories (floor ${MIN_GO_DIRS})." >&2
  echo "  The scan is broken, not the repo. NOTHING was verified." >&2
  exit 1
fi

# The specific hole this gate was written to close. `integration/` holds the
# control-plane seam and was outside the old hardcoded list; assert by name that
# it is inside the scope now, so a future refactor of the discovery cannot
# quietly reopen the same hole.
if ! printf '%s\n' "${go_dirs[@]}" | grep -q '^\./integration'; then
  echo "gofmt gate: FAIL — ./integration is not in the discovered scope." >&2
  echo "  That directory is the exact blind spot this gate exists to close." >&2
  exit 1
fi

echo "gofmt gate: scope = ${file_count} Go files across ${dir_count} directories:"
printf '  %s\n' "${go_dirs[@]}"

# --- the actual check --------------------------------------------------------
unformatted="$(gofmt -l "${go_files[@]}")"
if [ -n "${unformatted}" ]; then
  echo "" >&2
  echo "gofmt gate: FAIL — the following files are not gofmt-clean:" >&2
  printf '  %s\n' ${unformatted} >&2
  echo "" >&2
  echo "  Fix with: gofmt -w <file>   (or \`make fmt\`)" >&2
  exit 1
fi

echo "gofmt gate: PASS — all ${file_count} Go files are gofmt-clean."

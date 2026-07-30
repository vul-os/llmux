#!/usr/bin/env bash
#
# web/dist staleness gate — "the bundle you ship IS a build of the source you ship".
#
# web/dist is COMMITTED (see the note in .gitignore) and compiled into the
# gateway binary by web/embed.go's `go:embed all:dist`, so `go build` works with
# no Node toolchain. The consequence: the UI a user gets is the bundle in git,
# NOT the one CI just built in a scratch directory. Edit web/src — or web/docs,
# since Docs.jsx imports that markdown with `?raw` — without rebuilding, and
# every release ships the previous UI while every existing check stays green.
#
# That is not hypothetical here. Commit 630177c changed web/src; web/dist was
# not rebuilt until 2ec6674, two commits later. In between, HEAD shipped a
# bundle that contradicted its own source and nothing noticed.
#
# Adapted from the equivalent gate in the openrate repo (read-only reference —
# nothing imported), with four changes, per this programme's rule that a gate
# must fail closed or skip loudly:
#
#   * NO SKIP PATH. Missing npm/node/git, no module root, or a build that did
#     not produce a bundle is a FAILURE. A gate that shrugs is a gate that
#     passes by doing nothing; this programme has already found five of those.
#
#   * COVERAGE FLOORS, asserted before anything is compared: how many files are
#     tracked under web/dist, how many were hashed, how many assets index.html
#     referenced. An emptied dist/, a wrong path, or a build that wrote nowhere
#     therefore cannot register as "no differences found".
#
#   * CONTENT HASHES, not `git diff`. openrate compares the post-build tree to
#     git, which has two problems. It is blind to ADDED files (vite emits
#     content-hashed chunk names, so a stale bundle often shows up as a new
#     untracked chunk, not a modification), and it reports legitimate
#     uncommitted work — a src edit plus its matching rebuild — as staleness.
#     This snapshots web/dist, rebuilds, and compares the two snapshots. In CI
#     the pre-build snapshot IS the committed tree, so the check is identical
#     there, and it stays correct in a dirty working tree.
#
#   * BUILT IN AN ISOLATED COPY, not in place. An earlier version of this gate
#     ran `npm --prefix web run build` directly against the real working tree —
#     which overwrites the real, tracked web/dist (and regenerates the tracked
#     THIRD-PARTY-NOTICES*.txt, which `npm run build` also runs) as a side
#     effect of "checking" them. A read-only gate has no business mutating the
#     thing it verifies, on a pass OR a fail: this materializes the current
#     working tree (respecting .gitignore, so no node_modules/target/bin junk)
#     into a scratch copy and builds THERE, so the real tree is never touched
#     no matter what this gate finds.
#
# Usage:  ./scripts/check-web-dist.sh
#
set -euo pipefail

if [ -n "${1:-}" ]; then
  echo "web/dist gate: FAIL — unexpected argument '${1}' (this gate takes none)" >&2
  exit 2
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "${here}/.." && pwd)"
if [ ! -f "${root}/go.mod" ] || [ ! -f "${root}/web/embed.go" ]; then
  echo "web/dist gate: FAIL — ${root} is not the llmux module root; the gate compared NOTHING" >&2
  exit 1
fi
cd "${root}"

for tool in git npm rsync; do
  command -v "${tool}" >/dev/null 2>&1 || {
    echo "web/dist gate: FAIL — '${tool}' is not installed, so the committed bundle could NOT be" >&2
    echo "  rebuilt from web/src and its freshness is UNVERIFIED. Install git, rsync and Node" >&2
    echo "  20.19+/22.12+ (vite 8 requires it) and re-run." >&2
    exit 1
  }
done

if [ ! -d "web/node_modules" ]; then
  echo "web/dist gate: FAIL — web/node_modules is missing (run 'npm --prefix web install' first)." >&2
  echo "  Nothing was rebuilt or compared." >&2
  exit 1
fi

# --- coverage floor: the bundle must actually be committed -------------------
# 13 files are tracked under web/dist today (index.html, licenses.txt, 4 hashed
# assets, 6 icons/images, 1 screenshot). If that set collapses, the binary
# embeds nothing and every comparison below would trivially "pass".
MIN_TRACKED_FILES=10
tracked_count="$(git ls-files -- web/dist | wc -l | tr -d ' ')"
if [ "${tracked_count}" -lt "${MIN_TRACKED_FILES}" ]; then
  echo "web/dist gate: FAIL — only ${tracked_count} files are tracked under web/dist (floor ${MIN_TRACKED_FILES})." >&2
  echo "  The embedded bundle is missing or was un-committed. NOTHING meaningful was compared." >&2
  exit 1
fi

# --- manifest helper ---------------------------------------------------------
# A sorted "sha256  web/dist/<path>" listing, scanned from whichever tree root
# is passed in — the real one for "before", the scratch copy for "after" — so
# both manifests carry identical, comparable path labels.
sha_cmd=""
if command -v sha256sum >/dev/null 2>&1; then
  sha_cmd="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  sha_cmd="shasum -a 256"
else
  echo "web/dist gate: FAIL — neither sha256sum nor shasum is available; the bundle's contents" >&2
  echo "  could not be hashed and staleness was NOT checked." >&2
  exit 1
fi

manifest_at() {
  # shellcheck disable=SC2086
  ( cd "$1" && find web/dist -type f -print0 | sort -z | xargs -0 ${sha_cmd} )
}

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

manifest_at "${root}" > "${workdir}/before.txt"
before_count="$(wc -l < "${workdir}/before.txt" | tr -d ' ')"
if [ "${before_count}" -lt "${MIN_TRACKED_FILES}" ]; then
  echo "web/dist gate: FAIL — hashed only ${before_count} files in web/dist (floor ${MIN_TRACKED_FILES})." >&2
  echo "  The shipped bundle is empty or the scan is broken; NOTHING was compared." >&2
  exit 1
fi

# --- rebuild from source, in an ISOLATED copy --------------------------------
# `git ls-files --cached --others --exclude-standard` is exactly "every path
# git would not ignore" — tracked files as they currently sit on disk (so a
# legitimate uncommitted src edit is included, matching this gate's existing
# promise to stay correct in a dirty working tree) plus untracked-but-unignored
# files, and nothing under node_modules/target/bin/the-committed-dist-itself.
# Copying that list (not the whole directory) is what keeps this fast and
# keeps the scratch copy's own working tree free of build byproducts.
copy="${workdir}/tree"
mkdir -p "${copy}"
git ls-files -z --cached --others --exclude-standard | rsync -a --files-from=- --from0 "${root}/" "${copy}/"
# web/node_modules is git-ignored (so absent from the file list above) and is
# reused from the real tree rather than reinstalled. A real copy, not a
# symlink: the npm license scan (`license-checker-rseidelsohn`, driving
# notices:all) walks node_modules by realpath and silently resolves a
# symlinked node_modules back through to a DIFFERENT working directory,
# under-reporting the dependency graph (120 components -> 8, observed while
# building this fix) — a wrong notices scan is exactly the kind of silent
# under-verification this programme has already found five of elsewhere.
cp -R "${root}/web/node_modules" "${copy}/web/node_modules"

echo "web/dist gate: hashed ${before_count} shipped files; rebuilding web/dist from web/src in an isolated copy..."
npm --prefix "${copy}/web" run build >/dev/null

# The build must actually have produced the entry documents. If vite wrote
# nowhere, an unchanged manifest would mean nothing at all.
for required in dist/index.html dist/licenses.txt; do
  [ -s "${copy}/web/${required}" ] || {
    echo "web/dist gate: FAIL — web/${required} is missing or empty after the isolated build." >&2
    echo "  The build produced no bundle, so 'no differences' proves nothing." >&2
    exit 1
  }
done

manifest_at "${copy}" > "${workdir}/after.txt"
after_count="$(wc -l < "${workdir}/after.txt" | tr -d ' ')"
if [ "${after_count}" -lt "${MIN_TRACKED_FILES}" ]; then
  echo "web/dist gate: FAIL — the fresh build produced only ${after_count} files (floor ${MIN_TRACKED_FILES})." >&2
  echo "  A build that emits almost nothing cannot be used as a reference." >&2
  exit 1
fi

# --- dangling-reference check ------------------------------------------------
# Every asset URL in index.html must exist on disk. A half-updated bundle (new
# index.html, old content-hashed chunks, or the reverse) is a white screen, and
# it is the failure mode a partial commit produces. Checked against the fresh
# (isolated-copy) build, which is the one whose references are being asserted.
asset_refs=0
while IFS= read -r ref; do
  [ -n "${ref}" ] || continue
  asset_refs=$((asset_refs + 1))
  if [ ! -f "${copy}/web/dist/${ref}" ]; then
    echo "web/dist gate: FAIL — web/dist/index.html references '${ref}', which is not in the fresh build." >&2
    echo "  The bundle is incomplete; /ui would load as a blank page." >&2
    exit 1
  fi
done < <(grep -o 'assets/[A-Za-z0-9._-]\+' "${copy}/web/dist/index.html" | sort -u)

if [ "${asset_refs}" -lt 2 ]; then
  echo "web/dist gate: FAIL — index.html references ${asset_refs} bundled assets (expected at least" >&2
  echo "  2: a JS entry and a stylesheet). Either the parse broke or this is not a real build," >&2
  echo "  so the reference check verified nothing." >&2
  exit 1
fi

# --- the actual staleness verdict -------------------------------------------
if ! diff -u "${workdir}/before.txt" "${workdir}/after.txt" > "${workdir}/drift.txt"; then
  echo "" >&2
  echo "web/dist gate: FAIL — the shipped web/dist does NOT match a fresh build of web/src." >&2
  echo "  The binary embeds the SHIPPED bundle, so releases would serve this stale UI." >&2
  echo "  (- = what was shipped, + = what the source actually builds to)" >&2
  echo "" >&2
  sed 's/^/    /' "${workdir}/drift.txt" >&2
  echo "" >&2
  echo "  Fix: run 'make web' (npm --prefix web run build) and commit web/dist." >&2
  exit 1
fi

echo "web/dist gate: PASS — all ${after_count} files in web/dist are byte-identical to a fresh"
echo "  build of web/src, ${tracked_count} of them are committed, and all ${asset_refs} assets"
echo "  referenced by index.html are present."

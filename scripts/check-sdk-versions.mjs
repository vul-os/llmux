#!/usr/bin/env node
// =============================================================================
// scripts/check-sdk-versions.mjs — every package manifest under sdks/ declares
// the version in /VERSION. Fail-closed.
//
// WHY
// ───
// llmux tagged and published 0.1.7 while ALL NINE of its version-carrying
// manifests still said 0.1.0 — six releases of drift in bun, deno, node (and
// its lockfile), java, dotnet, rust, python and ruby.
// Nothing compared them, so nothing noticed. A published package whose manifest
// declares the wrong version is not a cosmetic problem: it is what a registry,
// a lockfile and a consumer's resolver all believe.
//
// sdks/elixir/mix.exs already had the right answer and had it in a comment —
// it reads ../../VERSION at build time "so this package cannot drift". Most of
// the other formats cannot do that: package.json, deno.json, pom.xml, .csproj
// and Cargo.toml all need a literal, because a registry has to read the version
// without running anything. Where a literal is forced, the fix is not a better
// literal, it is a check — the same conclusion openrate reached for its C
// header, which is generated from VERSION for exactly this reason.
//
// WHAT IT ASSERTS
//   1. Every manifest listed below exists and declares a version.
//   2. That version equals /VERSION exactly.
//   3. The list is complete: every directory under sdks/ either has a manifest
//      here or is named in NO_MANIFEST with a reason. A new SDK cannot be added
//      without this file being told, which is the failure mode that let six
//      manifests drift in the first place.
//
//   node scripts/check-sdk-versions.mjs             # check
//   node scripts/check-sdk-versions.mjs --selftest  # break it, require refusal
// =============================================================================

import { readFileSync, readdirSync, existsSync, writeFileSync } from 'node:fs';
import { resolve, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const SDKS = join(ROOT, 'sdks');
// This repo's own name, used to tell our release pins from a third party's.
const PROJECT = 'llmux';

// One entry per SDK that ships a manifest carrying a version literal.
// `read` returns the declared version, or null if the file does not declare one.
const MANIFESTS = [
  { sdk: 'bun', file: 'package.json', read: (s) => JSON.parse(s).version ?? null },
  { sdk: 'deno', file: 'deno.json', read: (s) => JSON.parse(s).version ?? null },
  { sdk: 'node', file: 'package.json', read: (s) => JSON.parse(s).version ?? null },
  { sdk: 'node', file: 'package-lock.json', read: (s) => JSON.parse(s).version ?? null },
  { sdk: 'java', file: 'pom.xml', read: (s) => (/<artifactId>llmux<\/artifactId>\s*<version>([^<]+)<\/version>/.exec(s) || [])[1] ?? null },
  { sdk: 'dotnet', file: 'Llmux.csproj', read: (s) => (/<Version>([^<]+)<\/Version>/.exec(s) || [])[1] ?? null },
  { sdk: 'rust', file: 'Cargo.toml', read: (s) => (/^\s*version\s*=\s*"([^"]+)"/m.exec(s) || [])[1] ?? null },
  { sdk: 'python', file: 'pyproject.toml', read: (s) => (/^\s*version\s*=\s*"([^"]+)"/m.exec(s) || [])[1] ?? null },
  { sdk: 'ruby', file: 'llmux.gemspec', read: (s) => (/\.version\s*=\s*"([^"]+)"/.exec(s) || [])[1] ?? null },
];

// SDKs with no version literal to check, and why. A reason, not a shrug: if one
// of these grows a manifest, the completeness check below still passes, so the
// reason is what a future reader has to disagree with before removing it.
const NO_MANIFEST = {
  elixir: 'mix.exs derives @version from ../../VERSION at build time — nothing to drift',
  c: 'a header and a Makefile; the version lives in llmux-ffi/include/llmux.h, generated from VERSION',
  cpp: 'header-only, no package manifest',
  swift: 'Package.swift carries no version — SwiftPM takes it from the git tag',
  kotlin: 'built by the Makefile against the cdylib; no published artifact yet',
  php: 'composer.json declares no version — Packagist takes it from the git tag',
  go: 'Go modules take their version from the git tag, never from a file',
};

const RED = process.stderr.isTTY ? '\x1b[31m' : '';
const GRN = process.stderr.isTTY ? '\x1b[32m' : '';
const RST = process.stderr.isTTY ? '\x1b[0m' : '';

function check(root) {
  const problems = [];
  const versionFile = join(root, 'VERSION');
  if (!existsSync(versionFile)) return [`no VERSION at ${versionFile} — this check verified NOTHING`];
  const want = readFileSync(versionFile, 'utf8').trim();
  if (!/^\d+\.\d+\.\d+/.test(want)) return [`VERSION is ${JSON.stringify(want)}, not a version — this check verified NOTHING`];

  let checked = 0;
  for (const m of MANIFESTS) {
    const p = join(root, 'sdks', m.sdk, m.file);
    if (!existsSync(p)) { problems.push(`sdks/${m.sdk}/${m.file} is missing — it is listed here, so it is expected to exist`); continue; }
    let got;
    try { got = m.read(readFileSync(p, 'utf8')); }
    catch (e) { problems.push(`sdks/${m.sdk}/${m.file} could not be parsed: ${e.message}`); continue; }
    if (got === null || got === undefined) { problems.push(`sdks/${m.sdk}/${m.file} declares no version — deleting the field is not a way to pass this check`); continue; }
    checked += 1;
    if (got !== want) problems.push(`sdks/${m.sdk}/${m.file} declares ${got} but VERSION says ${want}`);
  }
  if (checked < MANIFESTS.length) {
    problems.push(`only ${checked} of ${MANIFESTS.length} manifests were actually read — the rest are counted above`);
  }

  // Anything a reader is told to RUN that names a release tag must name this
  // one. docs/quickstart.md told people to fetch verify.sh from v0.1.3 and
  // check a v0.1.3 artifact, five releases after that stopped being the
  // release — while README.md, three files away, had v0.1.8. Nobody compared
  // them, and a stale pin here is worse than a stale sentence: the reader runs
  // it, and verifies the wrong bytes against the wrong manifest.
  //
  // Only COMMANDS and release URLs, and only THIS project's. Prose that dates
  // a change ("since 0.1.5", "until 0.1.5") is history and must stay as
  // written.
  //
  // Scoping to this project is not fussiness — the first version of this matched
  // any `--tag vX.Y.Z` and immediately failed patala on
  //
  //   cargo install --git .../NordSecurity/uniffi-bindgen-go --tag v0.5.0+v0.29.5
  //
  // which is a third party's tag and has nothing to do with the release. A
  // check that flags correct lines gets switched off, so each pattern below
  // either carries this repo's own path or must appear beside verify.sh, which
  // is ours.
  const SELF = new RegExp(`(?:vul-os/${PROJECT}\\b|\\bverify\\.sh\\b)`);
  const PINNED = [
    { re: new RegExp(`raw\\.githubusercontent\\.com/[^/]+/${PROJECT}/v(\\d+\\.\\d+\\.\\d+)`, 'g'), what: 'a raw.githubusercontent URL', scoped: true },
    { re: new RegExp(`releases/tag/v(\\d+\\.\\d+\\.\\d+)`, 'g'), what: 'a releases/tag link', scoped: false },
    { re: /--tag\s+v(\d+\.\d+\.\d+)(?![+\w.-])/g, what: 'a --tag argument', scoped: false },
    { re: new RegExp(`${PROJECT}_(\\d+\\.\\d+\\.\\d+)_`, 'g'), what: 'a release asset name', scoped: true },
  ];
  const docs = [];
  const readmeP = join(root, 'README.md');
  if (existsSync(readmeP)) docs.push(['README.md', readFileSync(readmeP, 'utf8')]);
  const docsDir = join(root, 'docs');
  if (existsSync(docsDir)) {
    for (const f of readdirSync(docsDir)) {
      if (f.endsWith('.md')) docs.push([`docs/${f}`, readFileSync(join(docsDir, f), 'utf8')]);
    }
  }
  if (docs.length < 3) problems.push(`only ${docs.length} docs read — the release-pin check verified NOTHING`);

  // Docs may reference the release with a PLACEHOLDER instead of a literal —
  // `--tag <tag>`, `llmux_<v>_source.zip`. patala does exactly that, and it
  // is the better choice where it reads well: a placeholder cannot go stale.
  // They count toward "this check examined something" without being compared
  // against anything, because there is nothing in them to be wrong.
  const PLACEHOLDER = /(?:--tag\s+<[a-z]+>|releases\/tag\/<[a-z]+>|\/<[a-z]+>\/scripts\/verify\.sh|_<[a-z]+>_)/g;
  let placeholders = 0;
  let pins = 0;
  for (const [name, text] of docs) {
    for (const { re, what, scoped } of PINNED) {
      for (const m of text.matchAll(re)) {
        // An unscoped pattern must prove it is talking about US: the same line
        // has to carry this repo's path or our own verify.sh.
        if (!scoped) {
          const lineStart = text.lastIndexOf('\n', m.index) + 1;
          let lineEnd = text.indexOf('\n', m.index);
          if (lineEnd < 0) lineEnd = text.length;
          // verify.sh is usually invoked on the line after it is fetched, so
          // allow the two lines above as context for that one signal.
          const ctxStart = Math.max(0, text.lastIndexOf('\n', Math.max(0, lineStart - 2)) - 200);
          if (!SELF.test(text.slice(ctxStart, lineEnd))) continue;
        }
        pins += 1;
        if (m[1] !== want) problems.push(`${name} pins ${what} at ${m[1]}, but the release is ${want} — a reader running that line fetches the wrong release`);
      }
    }
  }
  for (const [, text] of docs) placeholders += [...text.matchAll(PLACEHOLDER)].length;
  if (pins + placeholders === 0) {
    problems.push('no release reference — pinned or placeholder — was found in README.md or docs/; ' +
      'the release-pin check verified NOTHING');
  }

  // No documented command may install THIS project from a registry it is not
  // published to.
  //
  // The Python README said `pip install llmux`, which 404s. llmux's said
  // `pip install llmux`, which is worse: that name on PyPI belongs to an
  // unrelated project, so the documented first step installed a stranger's
  // package and then had the reader call our API on it. crates.io `llmux` is
  // taken too, by a same-category crate at 2.4.0.
  //
  // UNPUBLISHED is the claim being held, and it is meant to shrink: when a
  // package really is published, delete its entry here and the command becomes
  // legal. Checked 2026-08-10 against every registry below.
  // Both spellings: the vulos-scoped name we will publish under, and the bare
  // one we must never tell anyone to install. The bare name is not merely
  // unpublished — on PyPI and crates.io it belongs to unrelated projects, so
  // documenting it hands the reader somebody else's code.
  const NAME = `(?:vulos[-_.]?)?${PROJECT}`;
  const UNPUBLISHED = [
    { re: new RegExp(`pip install\\s+${NAME}\\b`, 'g'), registry: 'PyPI' },
    { re: new RegExp(`npm i(?:nstall)?\\s+(?:@vul-?os/)?${NAME}\\b`, 'gi'), registry: 'npm' },
    { re: new RegExp(`gem install\\s+${NAME}\\b`, 'g'), registry: 'RubyGems' },
    { re: new RegExp(`cargo add\\s+${NAME}\\b`, 'g'), registry: 'crates.io' },
    { re: new RegExp(`dotnet add package\\s+(?:Vulos\\.)?${PROJECT}\\b`, 'gi'), registry: 'NuGet' },
    { re: new RegExp(`composer require\\s+(?:vulos|${PROJECT})/${PROJECT}\\b`, 'g'), registry: 'Packagist' },
  ];

  const sdkDocs = [...docs];
  const sdksRoot = join(root, 'sdks');
  if (existsSync(sdksRoot)) {
    for (const d of readdirSync(sdksRoot, { withFileTypes: true })) {
      if (!d.isDirectory()) continue;
      const rp = join(sdksRoot, d.name, 'README.md');
      if (existsSync(rp)) sdkDocs.push([`sdks/${d.name}/README.md`, readFileSync(rp, 'utf8')]);
    }
  }
  if (sdkDocs.length <= docs.length) problems.push('no sdks/*/README.md was read — the registry-claim check verified NOTHING');
  let mentioned = 0; // prose mentions, deliberately not flagged
  for (const [name, text] of sdkDocs) {
    for (const { re, registry } of UNPUBLISHED) {
      for (const m of text.matchAll(re)) {
        // A command being DISCUSSED is not a command being prescribed, and the
        // difference is not a word list. The fix for this defect is a doc that
        // says "Not `pip install llmux`", and patala's says "There is no
        // `cargo add patala-core`" — both contain the exact string being
        // searched for. Matching on nearby negations caught the first and
        // missed the second, because "no" is not "not".
        //
        // What separates them is form, not vocabulary: an instruction is a
        // command in a fenced block, or a line that begins with the command.
        // A mention inside a sentence is prose about the command.
        const lineStart = text.lastIndexOf('\n', m.index) + 1;
        const fencesBefore = (text.slice(0, m.index).match(/^```/gm) || []).length;
        const inFence = fencesBefore % 2 === 1;
        const startsLine = /^\s*$/.test(text.slice(lineStart, m.index));
        if (!inFence && !startsLine) { mentioned += 1; continue; }
        problems.push(`${name} tells the reader to run "${m[0].trim()}", but ${PROJECT} is not published to ${registry}. ` +
          `If it now is, remove that registry from UNPUBLISHED in this file.`);
      }
    }
  }

  // Completeness: no SDK may be silently outside this check.
  const dirs = readdirSync(join(root, 'sdks'), { withFileTypes: true })
    .filter((e) => e.isDirectory() && !e.name.startsWith('.'))
    .map((e) => e.name);
  if (dirs.length < 10) problems.push(`only ${dirs.length} directories under sdks/ — the completeness check verified NOTHING`);
  const known = new Set([...MANIFESTS.map((m) => m.sdk), ...Object.keys(NO_MANIFEST)]);
  for (const d of dirs) {
    if (!known.has(d)) {
      problems.push(`sdks/${d} is neither in MANIFESTS nor in NO_MANIFEST — add it to one. ` +
        `Six manifests drifted to 0.1.0 behind a 0.1.2 release because nothing was watching them.`);
    }
  }
  return problems;
}

// ── selftest ────────────────────────────────────────────────────────────────
// Mutates a real manifest in place and restores it, because a copy of the tree
// would not prove this reads the tree that ships.
if (process.argv.includes('--selftest')) {
  const base = check(ROOT);
  if (base.length) {
    for (const p of base) process.stderr.write(`${RED}${p}${RST}\n`);
    process.stderr.write(`${RED}selftest: the UNMODIFIED tree already fails, so no mutation below proves anything${RST}\n`);
    process.exit(1);
  }
  const cases = [
    { name: 'a manifest drifts behind VERSION', file: join(SDKS, 'node/package.json'), edit: (s) => s.replace(/("version"\s*:\s*")[^"]+/, '$19.9.9') },
    { name: 'the lockfile drifts from its package.json', file: join(SDKS, 'node/package-lock.json'), edit: (s) => s.replace(/("version"\s*:\s*")[^"]+/, '$19.9.9') },
    { name: 'the pom version drifts', file: join(SDKS, 'java/pom.xml'), edit: (s) => s.replace(/(<artifactId>llmux<\/artifactId>\s*<version>)[^<]+/, '$19.9.9') },
    { name: 'a manifest deletes its version field', file: join(SDKS, 'deno/deno.json'), edit: (s) => s.replace(/"version"\s*:\s*"[^"]+",?\s*/, '') },
  ];
  let refused = 0;
  for (const c of cases) {
    const original = readFileSync(c.file, 'utf8');
    const mutated = c.edit(original);
    if (mutated === original) { process.stderr.write(`${RED}selftest: "${c.name}" changed nothing — the case proves nothing${RST}\n`); process.exit(1); }
    writeFileSync(c.file, mutated);
    const got = check(ROOT);
    writeFileSync(c.file, original);
    if (!got.length) {
      process.stderr.write(`${RED}selftest: "${c.name}" was NOT refused${RST}\n`);
      process.exit(1);
    }
    refused += 1;
    process.stderr.write(`${GRN}  refused${RST} ${c.name}\n`);
  }
  // And the completeness half.
  const newSdk = join(SDKS, 'zzz-selftest');
  const { mkdirSync, rmdirSync } = await import('node:fs');
  mkdirSync(newSdk, { recursive: true });
  const got = check(ROOT);
  rmdirSync(newSdk);
  if (!got.some((p) => p.includes('zzz-selftest'))) {
    process.stderr.write(`${RED}selftest: a new SDK directory outside the list was NOT refused${RST}\n`);
    process.exit(1);
  }
  refused += 1;
  process.stderr.write(`${GRN}  refused${RST} a new SDK that no one told this check about\n`);

  const after = check(ROOT);
  if (after.length) {
    for (const p of after) process.stderr.write(`${RED}${p}${RST}\n`);
    process.stderr.write(`${RED}selftest: the tree was left modified — restore failed${RST}\n`);
    process.exit(1);
  }
  process.stderr.write(`${GRN}selftest: ${refused}/${refused} deliberate breakages refused, tree restored clean${RST}\n`);
  process.exit(0);
}

const problems = check(ROOT);
if (problems.length) {
  process.stderr.write(`${RED}check-sdk-versions: FAIL — ${problems.length} problem(s)${RST}\n`);
  for (const p of problems) process.stderr.write(`${RED}  ✗ ${p}${RST}\n`);
  process.exit(1);
}
process.stderr.write(`${GRN}check-sdk-versions: OK — ${MANIFESTS.length} manifests all declare ${readFileSync(join(ROOT, 'VERSION'), 'utf8').trim()}${RST}\n`);

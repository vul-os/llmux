#!/usr/bin/env node
// =============================================================================
// scripts/check-sdk-versions.mjs — every package manifest under sdks/ declares
// the version in /VERSION. Fail-closed.
//
// WHY
// ───
// llmux tagged and published 0.1.2 while six of its fifteen package manifests
// still said 0.1.0 — bun, deno, node (and its lockfile), java, dotnet, rust.
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

#!/usr/bin/env node
// check-docs-chrome.test.mjs — mutation harness for scripts/check-docs-chrome.mjs.
//
// Usage:  node scripts/check-docs-chrome.test.mjs
//
// A gate is only evidence if it FAILS when its subject is broken. This harness
// copies the real site/ and docs/ trees into a throwaway directory, runs the
// gate against the copy unmutated (which must exit 0), then breaks one thing
// at a time and requires the gate to exit non-zero WITH the finding tag that
// belongs to the assertion under test. A mutation the gate survives is
// reported as FAIL and the harness exits non-zero.
//
// Zero dependencies — Node builtins only.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(here, '..');
const GATE = path.join(here, 'check-docs-chrome.mjs');

// Every mutation is applied to a THROWAWAY COPY of site/ and docs/, never to
// the tree you are working in — the gate takes --root precisely so this can be
// true. TMPDIR is honoured if set, so a CI runner with a constrained temp area
// still works.
const work = fs.mkdtempSync(path.join(os.tmpdir(), 'check-docs-chrome-'));

// ── build the throwaway copy ──────────────────────────────────────────────
fs.cpSync(path.join(REPO, 'site'), path.join(work, 'site'), { recursive: true });
fs.cpSync(path.join(REPO, 'docs'), path.join(work, 'docs'), { recursive: true });

const COPY_DOCS_HTML = path.join(work, 'site', 'docs.html');

function runGate() {
  const r = spawnSync(process.execPath, [GATE, '--root', work], { encoding: 'utf8' });
  return { code: r.status, out: (r.stdout || '') + (r.stderr || '') };
}

// ── mutation primitives ───────────────────────────────────────────────────
// Each mutation returns a restore() closure that puts the copy back exactly.
function editFile(file, mutate) {
  const original = fs.readFileSync(file, 'utf8');
  const next = mutate(original);
  if (next === original) throw new Error(`mutation was a no-op on ${file} — its anchor text is gone, so the mutation would prove nothing`);
  fs.writeFileSync(file, next);
  return () => fs.writeFileSync(file, original);
}
function replaceOnce(file, needle, replacement) {
  return editFile(file, (s) => {
    const i = s.indexOf(needle);
    if (i < 0) throw new Error(`anchor not found in ${path.relative(work, file)}: ${JSON.stringify(needle.slice(0, 70))}`);
    return s.slice(0, i) + replacement + s.slice(i + needle.length);
  });
}

// Mutate a declaration INSIDE a specific CSS rule. A flat string replace is not
// good enough here: `max-width:none;margin:0;` also appears on
// `.site-header>.wrap` earlier in the file, so a first-match replace mutated a
// rule the gate does not (and should not) care about and the mutation
// "survived" for the wrong reason. `probe` picks the right one of several rules
// sharing a selector — e.g. the desktop `.docs-shell` rather than the base one.
function replaceInRule(file, selRe, probe, needle, replacement) {
  return editFile(file, (s) => {
    const re = new RegExp(selRe.source, 'g');
    let m;
    while ((m = re.exec(s)) !== null) {
      const start = m.index + m[0].length;
      const end = s.indexOf('}', start); // declaration blocks hold no nested braces
      if (end < 0) continue;
      const body = s.slice(start, end);
      if (!body.includes(probe)) continue;
      const i = body.indexOf(needle);
      if (i < 0) continue;
      return s.slice(0, start) + body.slice(0, i) + replacement + body.slice(i + needle.length) + s.slice(end);
    }
    throw new Error(`no ${selRe} rule in ${path.relative(work, file)} containing ${JSON.stringify(probe)} and ${JSON.stringify(needle)}`);
  });
}

// ── the mutations ─────────────────────────────────────────────────────────
const mutations = [
  {
    name: 'assertion 1 — inject a <footer> into site/docs.html',
    expect: 'docs-footer',
    apply: () =>
      replaceOnce(
        COPY_DOCS_HTML,
        '</body>',
        '<footer class="site-footer"><div class="wrap">llmux</div></footer>\n</body>'
      ),
  },
  {
    name: 'assertion 2 — unpin the desktop docs rail (position:fixed -> position:static)',
    expect: 'docs-rail',
    apply: () =>
      // The DESKTOP rail rule specifically — identified by `top:var(--header-h)`,
      // which only the @media (min-width:1024px) rule carries. The phone
      // bottom-sheet rule in @media (max-width:1023px) is left pinned on
      // purpose: if the gate accepted "any .rail rule anywhere", this mutation
      // would survive.
      replaceInRule(COPY_DOCS_HTML, /\.rail\s*\{/, 'top:var(--header-h)', 'position:fixed', 'position:static'),
  },
  {
    name: 'assertion 2 — re-centre the docs shell (margin:0 -> margin:0 auto)',
    expect: 'docs-rail',
    apply: () =>
      replaceInRule(COPY_DOCS_HTML, /\.docs-shell\s*\{/, 'max-width:none', 'margin:0;', 'margin:0 auto;'),
  },
  {
    name: 'assertion 3 — add <script src="https://cdn.example.com/x.js">',
    expect: 'no-outbound',
    apply: () => replaceOnce(COPY_DOCS_HTML, '</body>', '<script src="https://cdn.example.com/x.js"></script>\n</body>'),
  },
  {
    name: 'assertion 3 — add <iframe src="https://example.com">',
    expect: 'no-outbound',
    apply: () => replaceOnce(COPY_DOCS_HTML, '</body>', '<iframe src="https://example.com"></iframe>\n</body>'),
  },
  {
    name: 'assertion 3 — add <link rel="stylesheet" href="https://fonts.example.com/x.css">',
    expect: 'no-outbound',
    apply: () =>
      replaceOnce(COPY_DOCS_HTML, '</head>', '<link rel="stylesheet" href="https://fonts.example.com/x.css">\n</head>'),
  },
  {
    name: 'assertion 3 — add @import of a remote stylesheet inside <style>',
    expect: 'no-outbound',
    // Bare `@import "…"` form — the url() form is already exercised by the
    // background-image mutation below, and they hit different branches.
    apply: () => replaceOnce(COPY_DOCS_HTML, '<style>', '<style>\n@import "https://cdn.example.com/reset.css";'),
  },
  {
    name: 'assertion 3 — add a fetch() to an absolute URL in page script',
    expect: 'no-outbound',
    apply: () =>
      replaceOnce(COPY_DOCS_HTML, '</body>', '<script>fetch("https://telemetry.example.com/beacon");</script>\n</body>'),
  },
  {
    name: 'assertion 3 — add a remote background-image url() inside <style>',
    expect: 'no-outbound',
    apply: () =>
      replaceOnce(COPY_DOCS_HTML, '<style>', '<style>\n.x{background-image:url(https://img.example.com/bg.png)}'),
  },
  {
    name: 'assertion 4 — add a ```haskell fence to a docs/*.md',
    expect: 'fence-lang',
    apply: () => {
      const target = path.join(work, 'docs', 'api.md');
      return editFile(target, (s) => s + '\n```haskell\nmain = putStrLn "unregistered"\n```\n');
    },
  },
  {
    name: 'coverage floor — empty the docs/ tree so zero fences are found',
    expect: 'coverage',
    apply: () => {
      const dir = path.join(work, 'docs');
      const saved = [];
      for (const name of fs.readdirSync(dir)) {
        const p = path.join(dir, name);
        if (fs.statSync(p).isFile() && name.toLowerCase().endsWith('.md')) {
          saved.push([p, fs.readFileSync(p)]);
          fs.rmSync(p);
        }
      }
      if (saved.length === 0) throw new Error('docs/ held no markdown to remove — the coverage mutation would prove nothing');
      return () => {
        for (const [p, buf] of saved) fs.writeFileSync(p, buf);
      };
    },
  },
  {
    name: 'coverage floor — truncate site/docs.html so the chrome checks have no subject',
    expect: 'coverage',
    apply: () => editFile(COPY_DOCS_HTML, () => '<!doctype html><title>x</title>\n'),
  },
];

// ── negative controls ─────────────────────────────────────────────────────
// The mirror image of a mutation: legitimate content the gate must NOT flag.
// Without these, "every mutation was caught" is also satisfied by a gate that
// fails on the mere presence of the string "https://", which would be useless
// on these pages (they carry GitHub and vulos.org links and a canonical URL).
const negatives = [
  {
    name: 'a plain <a href="https://github.com/…"> anchor is navigation, not a load',
    apply: () =>
      replaceOnce(COPY_DOCS_HTML, '</body>', '<a href="https://github.com/vul-os/llmux">source</a>\n</body>'),
  },
  {
    name: '<link rel="canonical"> / rel="alternate" are metadata, not fetches',
    apply: () =>
      replaceOnce(COPY_DOCS_HTML, '</head>', '<link rel="alternate" href="https://vulos.org/projects/llmux/">\n</head>'),
  },
  {
    name: 'a localhost curl sample inside a markdown fence is documentation',
    apply: () => {
      const target = path.join(work, 'docs', 'api.md');
      return editFile(
        target,
        (s) => s + '\n```bash\ncurl https://api.openai.com/v1/models -H "x: y"\ncurl http://localhost:4000/v1/models\n```\n'
      );
    },
  },
  {
    name: 'an untagged bare ``` fence is allowed',
    apply: () => {
      const target = path.join(work, 'docs', 'api.md');
      return editFile(target, (s) => s + '\n```\nplain text, no language tag\n```\n');
    },
  },
  {
    name: 'a <pre><code> sample containing a CDN script tag is documentation, not a load',
    apply: () =>
      replaceOnce(
        COPY_DOCS_HTML,
        '</body>',
        '<pre><code>&lt;script src="https://cdn.example.com/x.js"&gt;&lt;/script&gt;</code></pre>\n</body>'
      ),
  },
];

// ── run ───────────────────────────────────────────────────────────────────
let failures = 0;
const log = (s) => process.stdout.write(s + '\n');

log(`mutation harness: repo=${REPO}`);
log(`mutation harness: copy=${work}`);

// (a) baseline: the unmutated copy must pass, otherwise every "mutation
//     failed the gate" result below is meaningless.
const base = runGate();
if (base.code === 0) {
  log('PASS  baseline — unmutated copy exits 0');
  const summary = base.out.split('\n').find((l) => l.includes('checked '));
  if (summary) log(`      ${summary.trim()}`);
} else {
  failures++;
  log(`FAIL  baseline — unmutated copy exited ${base.code}; no mutation result below can be trusted`);
  log(base.out.replace(/^/gm, '      '));
}

for (const m of mutations) {
  let restore = null;
  try {
    restore = m.apply();
  } catch (e) {
    failures++;
    log(`FAIL  ${m.name} — could not apply mutation: ${e.message}`);
    continue;
  }
  const r = runGate();
  const mentions = r.out.includes(`[${m.expect}]`);
  if (r.code !== 0 && mentions) {
    const first = r.out.split('\n').find((l) => l.includes(`[${m.expect}]`)) || '';
    log(`PASS  ${m.name}`);
    log(`      caught: ${first.trim().slice(0, 170)}`);
  } else {
    failures++;
    if (r.code === 0) log(`FAIL  ${m.name} — SURVIVED: gate still exited 0`);
    else log(`FAIL  ${m.name} — gate failed (exit ${r.code}) but no [${m.expect}] finding; it caught something else`);
    log(r.out.replace(/^/gm, '      ').slice(0, 2000));
  }
  restore();

  // (c) the restored copy must pass again, or the next result is polluted.
  const after = runGate();
  if (after.code !== 0) {
    failures++;
    log(`FAIL  ${m.name} — restore did not return the copy to a passing state (exit ${after.code})`);
    log(after.out.replace(/^/gm, '      ').slice(0, 1200));
  }
}

for (const n of negatives) {
  let restore = null;
  try {
    restore = n.apply();
  } catch (e) {
    failures++;
    log(`FAIL  negative control: ${n.name} — could not apply: ${e.message}`);
    continue;
  }
  const r = runGate();
  if (r.code === 0) {
    log(`PASS  negative control: ${n.name}`);
  } else {
    failures++;
    log(`FAIL  negative control: ${n.name} — gate FALSE-POSITIVED (exit ${r.code})`);
    log(r.out.replace(/^/gm, '      ').slice(0, 1600));
  }
  restore();
}

log('');
if (failures) {
  log(`mutation harness: FAIL — ${failures} problem${failures === 1 ? '' : 's'} (copy left at ${work} for inspection)`);
  process.exit(1);
}
fs.rmSync(work, { recursive: true, force: true });
log(
  `mutation harness: PASS — all ${mutations.length} mutations were caught, ` +
    `and all ${negatives.length} negative controls stayed green`
);

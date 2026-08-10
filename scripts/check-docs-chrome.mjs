#!/usr/bin/env node
// check-docs-chrome.mjs — static gate over site/ (zero dependencies, Node builtins only).
//
// Usage:  node scripts/check-docs-chrome.mjs [--root DIR]
//
// --root defaults to the repo root inferred from this file's location
// (scripts/../). EVERY path this gate touches is resolved under that root, so
// the mutation harness (scripts/check-docs-chrome.test.mjs) can point it at a
// throwaway copy of the tree and get identical behaviour.
//
// Exit 0 = pass. Non-zero = a numbered list of findings on stderr.
//
// ── On coverage floors ────────────────────────────────────────────────────
// This repo's dominant defect is "a guard that prints PASS while examining
// none of its subject". So every assertion below is paired with a floor: if
// the set it scans comes back empty (no HTML pages, no CSS rules for the
// selector, no markdown, no fences, no registered languages) that is a
// FAILURE tagged [coverage] saying the check verified nothing — never a
// silent pass. The summary line names the count of each thing checked so a
// run that examined nothing is visibly different from a run that passed.
//
// Finding tags (stable — the mutation harness greps for them):
//   [coverage]     a check could not reach its subject
//   [docs-footer]  assertion 1 — no <footer in site/docs.html
//   [docs-rail]    assertion 2 — sidebar pinned, shell packed left
//   [no-outbound]  assertion 3 — no outbound origins under site/
//   [fence-lang]   assertion 4 — every fence language is registered in hljs
//   [version]      assertion 5 — the landing's version badge equals VERSION
//
// ── Relationship to the suite gate ────────────────────────────────────────
// vulos-cloud/scripts/check-suite-chrome.mjs is the RATIFIED cross-repo gate
// for suite chrome (one Vulos element in the top bar, one .vulos-foot line,
// "Vulos" nowhere else in the body, no licence text in the footer) and for the
// docs no-footer rule. It auto-discovers this repo and llmux passes it. It is
// the authority on those rules and nothing here reimplements them.
//
// Assertions 2, 3 and 4 below are the ones it does NOT cover, and are the
// reason this file exists at all.
//
// Assertion 1 (no docs footer) is the deliberate exception to "do not
// duplicate the suite gate", and the justification is a real incident rather
// than caution: a sibling product removed its docs footer correctly, a later
// site redesign reinstated it, and NOTHING NOTICED FOR WEEKS — because the
// suite gate lives in another repository and runs in no repo's CI. A rule
// enforced only from outside is a rule that is checked whenever someone
// remembers. This one line makes llmux's own CI refuse the regression on the
// commit that introduces it. The suite gate stays the authority on the wording
// of the rule; this is just the tripwire, in the repo that can trip it.

import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

// ── args ──────────────────────────────────────────────────────────────────
const argv = process.argv.slice(2);
let root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
for (let i = 0; i < argv.length; i++) {
  if (argv[i] === '--root') {
    if (!argv[i + 1]) die('--root needs a directory');
    root = path.resolve(argv[++i]);
  } else if (argv[i].startsWith('--root=')) {
    root = path.resolve(argv[i].slice('--root='.length));
  } else {
    die(`unknown argument: ${argv[i]}`);
  }
}
function die(msg) {
  process.stderr.write(`check-docs-chrome: ${msg}\n`);
  process.exit(2);
}

const SITE = path.join(root, 'site');
const DOCS_HTML = path.join(SITE, 'docs.html');
const HLJS_BUNDLE = path.join(SITE, 'assets', 'vendor', 'highlight.min.js');

const findings = [];
const fail = (tag, msg) => findings.push(`[${tag}] ${msg}`);
const rel = (p) => path.relative(root, p) || p;

// ── small helpers ─────────────────────────────────────────────────────────
function readText(p) {
  try {
    return fs.readFileSync(p, 'utf8');
  } catch (e) {
    return null;
  }
}
function walk(dir, out = []) {
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return out;
  }
  for (const e of entries.sort((a, b) => (a.name < b.name ? -1 : 1))) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (e.isFile()) out.push(p);
  }
  return out;
}
const lineOf = (text, index) => text.slice(0, index).split('\n').length;
// Blank out a region while preserving byte offsets AND line numbers, so every
// finding's reported line still points at the real line in the real file.
const blank = (s) => s.replace(/[^\n]/g, ' ');

// ── counters for the summary line ─────────────────────────────────────────
const counts = {
  htmlPages: 0,
  htmlScanned: 0,
  siteFiles: 0,
  cssRules: 0,
  railRules: 0,
  shellRules: 0,
  urlOccurrences: 0,
  mdDocs: 0,
  fences: 0,
  taggedFences: 0,
  untaggedFences: 0,
  languages: new Set(),
  hljsLanguages: 0,
  versionBadges: 0,
  version: '?',
};

// ══════════════════════════════════════════════════════════════════════════
// Assertion 1 — site/docs.html carries no <footer element.
//
// Ratified suite rule (docs pages have no footer) and it has regressed before.
// The match is a plain case-insensitive `<footer` TAG match: occurrences
// inside HTML comments or inside JS string literals are NOT excluded. That is
// deliberate — a `<footer` string inside the page's own JS is a footer one
// innerHTML away from being real, and the rule is cheap to keep absolute.
// A CSS selector like `.site-footer{...}` does not contain `<` and so does
// not trip this.
// ══════════════════════════════════════════════════════════════════════════
const docsHtml = readText(DOCS_HTML);
if (docsHtml === null) {
  fail('coverage', `could not read ${rel(DOCS_HTML)} — the footer and rail checks verified NOTHING`);
} else if (docsHtml.length < 2000) {
  // Floor: an empty/truncated file would sail through a pure absence check.
  fail('coverage', `${rel(DOCS_HTML)} is only ${docsHtml.length} bytes — too small to be the real docs page; the footer and rail checks verified nothing meaningful`);
} else {
  counts.htmlPages++;
  const re = /<footer\b/gi;
  let m;
  while ((m = re.exec(docsHtml)) !== null) {
    fail('docs-footer', `${rel(DOCS_HTML)}:${lineOf(docsHtml, m.index)} contains a <footer element — docs pages carry no footer (ratified suite rule)`);
  }
}

// ══════════════════════════════════════════════════════════════════════════
// Assertion 2 — the docs sidebar is pinned, and the docs shell is packed left.
//
// Selectors asserted on, all of which genuinely exist in site/docs.html:
//
//   .rail          the sidebar. The element is <aside class="rail"
//                  id="docsNav">, so the check first proves that element
//                  exists AND carries the class — otherwise a CSS rule for
//                  `.rail` would be asserting about nothing.
//   .docs-shell    the grid wrapper around the document (<div class="wrap
//                  docs-shell">).
//
// "Pinned": at least one `.rail` rule in a DESKTOP context (no @media at all,
// or an @media whose prelude has a min-width) must declare position:sticky or
// position:fixed. The desktop qualifier matters: this file also has a
// `.rail{position:fixed;...}` inside @media (max-width:1023px) for the phone
// bottom sheet, so a check that accepted "any .rail rule anywhere" would keep
// passing after the desktop rail was unpinned. Matching is whitespace
// -insensitive because the file is written with no spaces after colons.
//
// "Packed left": `.docs-shell` also carries `.wrap`, and `.wrap` is
// `max-width:var(--max);margin:0 auto` — i.e. the shell IS centred until the
// desktop rule cancels it. So the honest anchor is the cancellation, which is
// exactly what the file's own comment describes: the desktop `.docs-shell`
// rule must set `max-width:none` and a NON-auto `margin`, and must claim the
// fixed rail's column with a `padding-inline` that references `--rail-w`.
// Plus: no `.docs-shell` rule anywhere may reintroduce an auto horizontal
// margin or centre the grid with justify-content/justify-items:center.
// This is not vacuous — flipping `margin:0` to `margin:0 auto`, dropping
// `max-width:none`, or dropping the padding-inline each fails it.
// ══════════════════════════════════════════════════════════════════════════

// Parse the CSS out of the page's <style> blocks into declaration blocks,
// keeping each block's at-rule context so "desktop" can be distinguished from
// "phone". Comments are blanked first (offset-preserving) so prose that
// mentions `position:fixed` cannot satisfy an assertion.
function parseCssBlocks(html) {
  const blocks = [];
  const styleRe = /<style\b[^>]*>([\s\S]*?)<\/style>/gi;
  let sm;
  while ((sm = styleRe.exec(html)) !== null) {
    const cssStart = sm.index + sm[0].indexOf(sm[1]);
    const css = sm[1].replace(/\/\*[\s\S]*?\*\//g, blank);
    const stack = []; // { prelude, hadNested }
    let prelude = '';
    for (let i = 0; i < css.length; i++) {
      const ch = css[i];
      if (ch === '{') {
        stack.push({ prelude: prelude.trim(), start: i + 1, hadNested: false });
        prelude = '';
      } else if (ch === '}') {
        const frame = stack.pop();
        if (!frame) continue;
        if (stack.length) stack[stack.length - 1].hadNested = true;
        if (!frame.hadNested) {
          blocks.push({
            selector: frame.prelude,
            atContext: stack.map((f) => f.prelude),
            decls: css.slice(frame.start, i),
            line: lineOf(html, cssStart + frame.start),
          });
        }
        prelude = '';
      } else {
        prelude += ch;
      }
    }
  }
  return blocks;
}

// Whitespace-insensitive declaration text: spaces around `:`, `;` and `,` are
// dropped (the file is written with no space after colons in most places, but
// not all), while a space INSIDE a value is preserved — otherwise `margin:0
// auto` squashes to `margin:0auto` and a check for the word `auto` misses it.
// That exact bug let the "re-centre the shell" mutation survive once.
const squash = (s) =>
  s.replace(/\s+/g, ' ').replace(/\s*([:;,])\s*/g, '$1').trim().toLowerCase();
const hasSelector = (selectorList, name) =>
  selectorList
    .split(',')
    .some((s) => new RegExp(`(^|[\\s>+~])\\.${name}(?![\\w-])`).test(' ' + s.trim()));

if (docsHtml) {
  const blocks = parseCssBlocks(docsHtml);
  counts.cssRules = blocks.length;
  if (blocks.length === 0) {
    fail('coverage', `parsed 0 CSS rules out of ${rel(DOCS_HTML)} — the rail/shell checks verified NOTHING`);
  }

  // The element the CSS is supposed to be about must actually exist.
  const navEl = /<([a-zA-Z][\w-]*)\b[^>]*\bid=["']docsNav["'][^>]*>/i.exec(docsHtml);
  if (!navEl) {
    fail('docs-rail', `${rel(DOCS_HTML)} has no element with id="docsNav" — there is no docs sidebar to pin`);
  } else if (!/\bclass=["'][^"']*\brail\b[^"']*["']/i.test(navEl[0])) {
    fail('docs-rail', `${rel(DOCS_HTML)}:${lineOf(docsHtml, navEl.index)} — #docsNav does not carry the "rail" class, so the .rail CSS rules do not apply to it`);
  }

  // ── pinned ──
  const railRules = blocks.filter((b) => hasSelector(b.selector, 'rail'));
  counts.railRules = railRules.length;
  const isDesktop = (b) =>
    b.atContext.length === 0 || b.atContext.every((a) => !/max-width/i.test(a)) ;
  const desktopRail = railRules.filter(isDesktop);
  if (railRules.length === 0) {
    fail('coverage', `found 0 CSS rules for the docs rail (\`.rail\`) in ${rel(DOCS_HTML)} — the "sidebar is pinned" check verified NOTHING`);
  } else if (desktopRail.length === 0) {
    fail('coverage', `found ${railRules.length} \`.rail\` rules but none in a desktop context (unconditional or @media min-width) in ${rel(DOCS_HTML)} — the "sidebar is pinned" check verified NOTHING`);
  } else {
    const pinned = desktopRail.filter((b) => /position:(sticky|fixed)(;|$|[^\w-])/.test(squash(b.decls) + ';'));
    if (pinned.length === 0) {
      const seen = desktopRail
        .map((b) => `${rel(DOCS_HTML)}:${b.line} ${b.atContext.join(' ')} { ${squash(b.decls).slice(0, 80)} }`)
        .join('; ');
      fail('docs-rail', `no desktop \`.rail\` rule declares position:sticky or position:fixed — the docs sidebar is not pinned. Rules examined: ${seen}`);
    }
  }

  // ── packed left ──
  const shellRules = blocks.filter((b) => hasSelector(b.selector, 'docs-shell'));
  counts.shellRules = shellRules.length;
  if (shellRules.length === 0) {
    fail('coverage', `found 0 CSS rules for \`.docs-shell\` in ${rel(DOCS_HTML)} — the "shell packed left" check verified NOTHING`);
  } else {
    // (a) nothing may centre the shell with an auto horizontal margin…
    for (const b of shellRules) {
      const d = squash(b.decls);
      const autoMargin =
        /(^|;)margin:[^;]*\bauto\b/.test(d) ||
        /(^|;)margin-inline:[^;]*\bauto\b/.test(d) ||
        /(^|;)margin-left:auto/.test(d) ||
        /(^|;)margin-right:auto/.test(d);
      if (autoMargin) {
        fail('docs-rail', `${rel(DOCS_HTML)}:${b.line} — \`${b.selector}\` sets an auto horizontal margin (${squash(b.decls).slice(0, 60)}); the docs shell must be packed left, not centred`);
      }
      if (/(justify-content|justify-items|place-content|place-items):center/.test(d)) {
        fail('docs-rail', `${rel(DOCS_HTML)}:${b.line} — \`${b.selector}\` centres its grid (justify*:center); the docs shell must be packed left`);
      }
    }
    // (b) …and the desktop rule must actively cancel the centred `.wrap`
    //     it shares an element with, and claim the fixed rail's column.
    const desktopShell = shellRules.filter((b) => b.atContext.some((a) => /min-width/i.test(a)));
    if (desktopShell.length === 0) {
      fail('coverage', `found ${shellRules.length} \`.docs-shell\` rules but none inside an @media min-width block in ${rel(DOCS_HTML)} — the "cancels the centred .wrap" check verified NOTHING`);
    } else {
      const cancels = desktopShell.some((b) => {
        const d = squash(b.decls);
        return /(^|;)max-width:none/.test(d) && /(^|;)margin:0(;|$)/.test(d);
      });
      if (!cancels) {
        fail('docs-rail', `no desktop \`.docs-shell\` rule cancels the centred \`.wrap\` with \`max-width:none\` + \`margin:0\` in ${rel(DOCS_HTML)} — the shell will be centred by .wrap{margin:0 auto}`);
      }
      const clearsRail = desktopShell.some((b) => /padding-inline:[^;]*--rail-w/.test(squash(b.decls)));
      if (!clearsRail) {
        fail('docs-rail', `no desktop \`.docs-shell\` rule claims the fixed rail's column with a \`padding-inline\` referencing \`--rail-w\` in ${rel(DOCS_HTML)} — the document would sit under the rail instead of starting where it ends`);
      }
    }
  }
}

// ══════════════════════════════════════════════════════════════════════════
// Assertion 3 — no outbound origins anywhere under site/.
//
// Scope: every .html, .css, .js, .svg and .md file under site/, INCLUDING
// site/assets/vendor/* — the vendored bundles are shipped bytes and are
// exactly where a CDN call would hide, so excluding them would gut the check.
//
// What fails: an absolute http/https/ws/wss URL reached as a SUBRESOURCE or a
// network call — src=/srcset= on script/img/iframe/source/embed/… , href= on
// <link> with a fetching rel (or no rel) and on <base>/<use>/<image>,
// @import, url(), fetch()/importScripts()/new WebSocket()/new EventSource()/
// new Worker(), and XHR .open('GET', 'https://…').
//
// What does NOT fail, and why:
//  · Plain anchors — <a href="https://github.com/…">, <a href="https://vulos.org">
//    are navigation, not loads, and are legitimate on these pages.
//  · <link rel="canonical"> (and alternate/author/license/me) — metadata the
//    page never fetches. Fetching rels (stylesheet, preload, prefetch,
//    preconnect, dns-prefetch, icon, manifest, modulepreload) DO fail, and so
//    does a <link> with no rel at all.
//  · XML namespace identifiers (http://www.w3.org/…, and the usual
//    inkscape/sodipodi/dc/cc/adobe namespaces in SVG) — opaque strings that no
//    browser ever dereferences.
//  · Anything inside <pre>/<code> in HTML/SVG, or inside a markdown fence or
//    inline `code` span. `curl http://localhost:4000/v1` and
//    `base_url="https://api.openai.com/v1"` in the docs are DOCUMENTATION of
//    what the reader runs, not loads the page performs. Those regions are
//    blanked out (offset- and line-preserving) before scanning.
//
// <iframe: any occurrence at all fails — in .html, .svg and .md. It is NOT a
// blanket rule for .js, because site/assets/vendor/mermaid.min.js contains a
// `<iframe … src="data:text/html;base64,…" sandbox>` template string used only
// when mermaid runs with securityLevel:"sandbox" (docs.html initialises it
// with securityLevel:'loose', so that path is dead), and a data: URI is not an
// outbound origin anyway. A JS-embedded iframe with an http(s) src still fails
// via the URL scan below.
// ══════════════════════════════════════════════════════════════════════════
const SCAN_EXT = new Set(['.html', '.htm', '.css', '.js', '.mjs', '.svg', '.md']);
const MARKUP_EXT = new Set(['.html', '.htm', '.svg', '.md']);
const NAMESPACE_PREFIXES = [
  'http://www.w3.org/',
  'https://www.w3.org/',
  'http://www.inkscape.org/namespaces/',
  'http://sodipodi.sourceforge.net/',
  'http://purl.org/dc/',
  'http://creativecommons.org/ns#',
  'http://ns.adobe.com/',
];
const SUBRESOURCE_TAGS = new Set([
  'script', 'img', 'iframe', 'frame', 'embed', 'object', 'source', 'track',
  'video', 'audio', 'image', 'use', 'input', 'portal',
]);
const FETCHING_RELS = /\b(stylesheet|preload|prefetch|preconnect|dns-prefetch|icon|apple-touch-icon|manifest|modulepreload|prerender)\b/i;

// Blank the regions of `text` that the regex matches *in `mask`* — mask is a
// copy of text with distracting regions already blanked, so `<pre` mentioned
// inside a CSS or JS comment cannot open a bogus region. Offsets and lines are
// identical between the two, so the ranges transfer directly.
function blankVia(text, mask, re) {
  let out = text;
  let m;
  re.lastIndex = 0;
  while ((m = re.exec(mask)) !== null) {
    out = out.slice(0, m.index) + blank(m[0]) + out.slice(m.index + m[0].length);
    if (m[0].length === 0) re.lastIndex++;
  }
  return out;
}

function stripCodeRegions(text, ext) {
  let out = text;
  if (ext === '.html' || ext === '.htm' || ext === '.svg') {
    // Mask the INNER text of <style> and <script> first. Both pages carry long
    // CSS and JS comments that discuss `<pre>` and `<code>` in prose; matched
    // naively those unpaired mentions open regions that swallowed 48% of
    // docs.html and 37% of index.html, i.e. most of the file would have gone
    // unscanned. The mask is only used to FIND the pre/code ranges — the
    // style/script text itself stays in the scanned copy, because @import,
    // url() and fetch() live there and must be checked.
    let mask = text;
    for (const re of [/(<style\b[^>]*>)([\s\S]*?)(<\/style>)/gi, /(<script\b[^>]*>)([\s\S]*?)(<\/script>)/gi]) {
      mask = mask.replace(re, (_all, open, inner, close) => open + blank(inner) + close);
    }
    // HTML comments are not rendered and load nothing.
    mask = blankVia(mask, mask, /<!--[\s\S]*?-->/g);
    out = blankVia(out, mask, /<!--[\s\S]*?-->/g);
    out = blankVia(out, mask, /<pre\b[\s\S]*?<\/pre>/gi);
    out = blankVia(out, mask, /<code\b[\s\S]*?<\/code>/gi);
  } else if (ext === '.md') {
    out = out.replace(/^([ \t]{0,3})(`{3,}|~{3,})[\s\S]*?^\1?\2[^\n]*$/gm, blank);
    // an unterminated fence swallows the rest of the file
    out = out.replace(/^([ \t]{0,3})(`{3,}|~{3,})[\s\S]*$/m, blank);
    out = out.replace(/(`+)[^\n]*?\1/g, blank);
    out = out.replace(/^(?: {4}|\t)[^\n]*$/gm, blank); // indented code blocks
  }
  return out;
}

const siteFiles = walk(SITE).filter((p) => SCAN_EXT.has(path.extname(p).toLowerCase()));
counts.siteFiles = siteFiles.length;
if (!fs.existsSync(SITE)) {
  fail('coverage', `no site/ directory under ${root} — the outbound-origin check verified NOTHING`);
} else if (siteFiles.length === 0) {
  fail('coverage', `found 0 scannable files (.html/.css/.js/.svg/.md) under ${rel(SITE)} — the outbound-origin check verified NOTHING`);
} else if (!siteFiles.some((p) => /\.html?$/i.test(p))) {
  fail('coverage', `found no HTML pages under ${rel(SITE)} — the outbound-origin check verified NOTHING for markup`);
}

const URL_RE = /\b(?:https?|wss?):\/\/[^\s"'`<>)\\]*/gi;
for (const file of siteFiles) {
  const ext = path.extname(file).toLowerCase();
  const raw = readText(file);
  if (raw === null) {
    fail('coverage', `could not read ${rel(file)} — it was skipped by the outbound-origin check`);
    continue;
  }
  if (/\.html?$/i.test(ext)) counts.htmlScanned++;
  const text = stripCodeRegions(raw, ext);

  if (MARKUP_EXT.has(ext)) {
    const ifr = /<iframe\b/gi;
    let im;
    while ((im = ifr.exec(text)) !== null) {
      fail('no-outbound', `${rel(file)}:${lineOf(text, im.index)} contains an <iframe — the site embeds no third-party frames`);
    }
  }

  URL_RE.lastIndex = 0;
  let m;
  while ((m = URL_RE.exec(text)) !== null) {
    const url = m[0];
    counts.urlOccurrences++;
    if (NAMESPACE_PREFIXES.some((p) => url.startsWith(p))) continue;
    const before = text.slice(Math.max(0, m.index - 240), m.index);
    const line = lineOf(text, m.index);
    const at = `${rel(file)}:${line}`;

    if (/\burl\(\s*['"]?$/i.test(before)) {
      fail('no-outbound', `${at} loads ${url} via CSS url() — assets must be local`);
      continue;
    }
    if (/@import\s+(url\(\s*)?['"]?$/i.test(before)) {
      fail('no-outbound', `${at} @imports ${url} — stylesheets must be local`);
      continue;
    }
    if (/\b(?:fetch|importScripts)\s*\(\s*['"`]$/.test(before)) {
      fail('no-outbound', `${at} calls out to ${url} — the site makes no outbound requests`);
      continue;
    }
    if (/new\s+(?:WebSocket|EventSource|SharedWorker|Worker)\s*\(\s*['"`]$/.test(before)) {
      fail('no-outbound', `${at} opens a connection to ${url} — the site makes no outbound requests`);
      continue;
    }
    if (/\.open\s*\(\s*['"][A-Za-z]+['"]\s*,\s*['"`]$/.test(before) || /XMLHttpRequest[\s\S]{0,200}\.open\s*\(\s*['"`]$/.test(before)) {
      fail('no-outbound', `${at} issues an XHR to ${url} — the site makes no outbound requests`);
      continue;
    }

    const attr = /\b(src|srcset|href|data-src|xlink:href)\s*=\s*['"]?$/i.exec(before);
    if (!attr) continue; // prose, a comment, an error string: not a load
    const tagStart = before.lastIndexOf('<');
    const tagText = tagStart >= 0 ? before.slice(tagStart) : '';
    const tag = (/^<\s*([a-zA-Z][\w:-]*)/.exec(tagText) || [, ''])[1].toLowerCase();
    const kind = attr[1].toLowerCase();

    if (kind === 'src' || kind === 'srcset' || kind === 'data-src') {
      if (SUBRESOURCE_TAGS.has(tag) || tag === '') {
        fail('no-outbound', `${at} <${tag || '?'} ${kind}> loads ${url} — subresources must be local`);
      }
      continue;
    }
    // href / xlink:href
    if (tag === 'link') {
      const relAttr = /\brel\s*=\s*["']([^"']*)["']/i.exec(tagText);
      if (!relAttr) {
        fail('no-outbound', `${at} <link href> with no rel points at ${url} — a <link> without a rel is treated as a fetch`);
      } else if (FETCHING_RELS.test(relAttr[1])) {
        fail('no-outbound', `${at} <link rel="${relAttr[1]}"> fetches ${url} — subresources must be local`);
      }
      // rel=canonical / alternate / author / license / me: metadata, not a load.
      continue;
    }
    if (tag === 'base' || tag === 'use' || tag === 'image' || tag === 'script') {
      fail('no-outbound', `${at} <${tag} href> resolves against ${url} — must be local`);
      continue;
    }
    // <a>, <area>: navigation. Legitimate.
  }
}

// ══════════════════════════════════════════════════════════════════════════
// Assertion 4 — every fenced code-block language in the docs is registered in
// the vendored highlight.js bundle.
//
// The bundle is a UMD build; loaded in a node:vm context with a stub global
// so it has somewhere to attach. `hljs` lands on the context itself (and, in
// this build, on ctx.self) — both are checked.
//
// Sources scanned: docs/*.md (the canonical tree) AND site/docs/*.md (the
// generated bundle the viewer actually fetches — it also carries roadmap.md
// and changelog.md, which are generated from ROADMAP.md / CHANGELOG.md at the
// repo root and therefore never appear in docs/). Both must be non-empty.
//
// Untagged fences (bare ```) are allowed and counted separately. `mermaid` is
// allowed explicitly: it is rendered by the vendored mermaid.js, not by
// highlight.js.
// ══════════════════════════════════════════════════════════════════════════
const EXTRA_FENCE_LANGS = new Set(['mermaid']);

let hljs = null;
const hljsSrc = readText(HLJS_BUNDLE);
if (hljsSrc === null) {
  fail('coverage', `could not read ${rel(HLJS_BUNDLE)} — the fence-language check verified NOTHING`);
} else if (hljsSrc.length < 10000) {
  fail('coverage', `${rel(HLJS_BUNDLE)} is only ${hljsSrc.length} bytes — not a real highlight.js bundle; the fence-language check verified nothing meaningful`);
} else {
  try {
    const ctx = { window: {}, self: {}, console };
    ctx.globalThis = ctx;
    vm.createContext(ctx);
    vm.runInContext(hljsSrc, ctx, { filename: rel(HLJS_BUNDLE), timeout: 20000 });
    hljs = ctx.hljs || ctx.window.hljs || ctx.self.hljs || null;
  } catch (e) {
    fail('coverage', `evaluating ${rel(HLJS_BUNDLE)} threw (${e.message}) — the fence-language check verified NOTHING`);
  }
  if (!hljs || typeof hljs.getLanguage !== 'function' || typeof hljs.listLanguages !== 'function') {
    fail('coverage', `no usable hljs object came out of ${rel(HLJS_BUNDLE)} — the fence-language check verified NOTHING`);
    hljs = null;
  } else {
    counts.hljsLanguages = hljs.listLanguages().length;
    if (counts.hljsLanguages === 0) {
      fail('coverage', `${rel(HLJS_BUNDLE)} registers 0 languages — every fence would be reported as unregistered or nothing would be checked at all`);
      hljs = null;
    }
  }
}

// Fence scanner: CommonMark-ish. An opener is up to 3 spaces of indent then 3+
// backticks or tildes; the info string's first word is the language. A closer
// is the same character, at least as long, with nothing else on the line.
function scanFences(text) {
  const out = [];
  const lines = text.split('\n');
  let open = null;
  for (let i = 0; i < lines.length; i++) {
    const m = /^ {0,3}(`{3,}|~{3,})(.*)$/.exec(lines[i]);
    if (!m) continue;
    if (!open) {
      const info = m[2].trim();
      if (m[1][0] === '`' && info.includes('`')) continue; // not a valid opener
      const lang = (info.split(/[\s,{]/)[0] || '').toLowerCase();
      open = { char: m[1][0], len: m[1].length, lang, line: i + 1 };
    } else if (m[1][0] === open.char && m[1].length >= open.len && m[2].trim() === '') {
      out.push(open);
      open = null;
    }
  }
  if (open) out.push({ ...open, unterminated: true });
  return out;
}

const mdDirs = [
  { dir: path.join(root, 'docs'), label: 'docs/' },
  { dir: path.join(SITE, 'docs'), label: 'site/docs/' },
];
let anyMd = false;
for (const { dir, label } of mdDirs) {
  let files = [];
  try {
    files = fs
      .readdirSync(dir, { withFileTypes: true })
      .filter((e) => e.isFile() && e.name.toLowerCase().endsWith('.md'))
      .map((e) => path.join(dir, e.name))
      .sort();
  } catch {
    files = [];
  }
  if (files.length === 0) {
    fail('coverage', `found 0 markdown files in ${label} — the code-fence language check verified NOTHING for that tree`);
    continue;
  }
  anyMd = true;
  counts.mdDocs += files.length;
  for (const file of files) {
    const text = readText(file);
    if (text === null) {
      fail('coverage', `could not read ${rel(file)} — its fences were not checked`);
      continue;
    }
    for (const f of scanFences(text)) {
      counts.fences++;
      if (f.unterminated) {
        fail('fence-lang', `${rel(file)}:${f.line} opens a code fence that is never closed`);
      }
      if (!f.lang) {
        counts.untaggedFences++;
        continue;
      }
      counts.taggedFences++;
      counts.languages.add(f.lang);
      if (EXTRA_FENCE_LANGS.has(f.lang)) continue;
      if (!hljs) continue; // already reported as a coverage failure
      if (!hljs.getLanguage(f.lang)) {
        fail('fence-lang', `${rel(file)}:${f.line} uses fence language \`${f.lang}\`, which is not registered in ${rel(HLJS_BUNDLE)} (${counts.hljsLanguages} languages) and is not an allowed non-hljs renderer (${[...EXTRA_FENCE_LANGS].join(', ')}) — it will render unhighlighted`);
      }
    }
  }
}
if (anyMd && counts.fences === 0) {
  fail('coverage', `scanned ${counts.mdDocs} markdown docs and found 0 code fences — the fence-language check verified NOTHING`);
}
if (counts.fences > 0 && counts.taggedFences === 0) {
  fail('coverage', `found ${counts.fences} code fences but not one carries a language tag — the fence-language check verified NOTHING`);
}

// ══════════════════════════════════════════════════════════════════════════
// Assertion 5 — the landing's version badge equals VERSION.
//
// The landing said v0.1.1 while VERSION said 0.1.2 and the release notes, the
// README and the C ABI's own version probe all said 0.1.2. Nobody noticed,
// because a version badge is the kind of string every reviewer's eye treats as
// furniture. It is the cheapest possible check and it caught a live defect the
// first time it ran.
//
// The badge is matched as a standalone `v<semver>` token in the landing's top
// rail. If the rail stops carrying one, that is a [coverage] failure, not a
// pass — otherwise deleting the badge would "fix" this check.
// ══════════════════════════════════════════════════════════════════════════
const VERSION_FILE = path.join(root, 'VERSION');
const INDEX_HTML = path.join(root, 'site', 'index.html');
const versionRaw = readText(VERSION_FILE);
const indexHtml = readText(INDEX_HTML);
if (versionRaw === null) {
  fail('coverage', `could not read ${rel(VERSION_FILE)} — the version-badge check verified NOTHING`);
} else if (indexHtml === null) {
  fail('coverage', `could not read ${rel(INDEX_HTML)} — the version-badge check verified NOTHING`);
} else {
  const want = versionRaw.trim();
  if (!/^\d+\.\d+\.\d+/.test(want)) {
    fail('coverage', `${rel(VERSION_FILE)} does not look like a version (${JSON.stringify(want.slice(0, 40))}) — the version-badge check verified NOTHING`);
  } else {
    const railRe = /<div class="toprail[^"]*"[^>]*>([\s\S]*?)<\/div>/i;
    const rail = railRe.exec(indexHtml);
    if (!rail) {
      fail('coverage', `no .toprail block in ${rel(INDEX_HTML)} — the version-badge check verified NOTHING`);
    } else {
      const badges = [...rail[1].matchAll(/>\s*v(\d+\.\d+\.\d+[^<\s]*)\s*</gi)].map((m) => m[1]);
      if (badges.length === 0) {
        fail('coverage', `the .toprail in ${rel(INDEX_HTML)} carries no v<semver> badge — the version-badge check verified NOTHING (deleting the badge is not a fix)`);
      }
      for (const got of badges) {
        if (got !== want) {
          fail('version', `${rel(INDEX_HTML)} advertises v${got} but ${rel(VERSION_FILE)} says ${want} — the landing is showing the wrong release`);
        }
      }
      counts.versionBadges = badges.length;
      counts.version = want;
    }
  }
}

// ══════════════════════════════════════════════════════════════════════════
// CH·03's package ledger must agree with sdks/
// ══════════════════════════════════════════════════════════════════════════
// "Fifteen languages" is a headline claim, and it is asserted in four places
// that have no mechanical relationship to each other: the <h2>, the chapter
// meta, the per-row counters in the ledger, and the number of links under them.
// Only one of those is checkable against the repo — the links — and the rest
// have to agree with it.
//
// This is the divergence class that keeps turning up here: a claim in prose
// drifting from the thing it describes, silently, because nothing compares
// them. Adding a sixteenth SDK and forgetting the landing is a one-line
// mistake that no other gate in this repo would notice.
//
// The counts are compared, not the names: the link slugs are documentation
// anchors (#sdks~c-header-only is C++) and do not map onto directory names.
{
  const SDKS = path.join(root, 'sdks');
  let dirs = [];
  try {
    dirs = fs.readdirSync(SDKS, { withFileTypes: true })
      .filter((e) => e.isDirectory() && !e.name.startsWith('.'))
      .map((e) => e.name);
  } catch {
    dirs = [];
  }

  const rowRe = /<span class="n">[\s\S]*?<span class="c">(\d+)<\/span>[\s\S]*?<div class="pkgrow">([\s\S]*?)<\/div>/g;
  const rows = [...(indexHtml || '').matchAll(rowRe)]
    .map((m) => ({ declared: Number(m[1]), links: [...m[2].matchAll(/class="pk\b[^"]*"/g)].length }));

  if (dirs.length < 10) {
    fail('coverage', `only ${dirs.length} package directories under ${rel(SDKS)} — the SDK-ledger check verified NOTHING`);
  } else if (rows.length < 3) {
    fail('coverage', `found ${rows.length} ledger rows in ${rel(INDEX_HTML)} (want at least 3) — the SDK-ledger check verified NOTHING`);
  } else {
    let listed = 0;
    for (const [i, r] of rows.entries()) {
      if (r.links === 0) {
        fail('coverage', `ledger row ${i + 1} in ${rel(INDEX_HTML)} lists no packages — the SDK-ledger check verified NOTHING for it`);
      }
      if (r.declared !== r.links) {
        fail('sdks', `ledger row ${i + 1} in ${rel(INDEX_HTML)} says ${r.declared} but lists ${r.links} package link(s)`);
      }
      listed += r.links;
    }
    if (listed !== dirs.length) {
      fail('sdks', `${rel(INDEX_HTML)} lists ${listed} packages but ${rel(SDKS)} ships ${dirs.length} ` +
        `(${dirs.sort().join(', ')}) — the landing and the repo disagree about how many languages there are`);
    }

    // The chapter meta restates the same split ("7 direct · 7 sidecar · 1
    // either"); if it carries numbers at all they have to add up to the rest.
    const ch03 = /<span class="ch">CH·03<\/span>[\s\S]{0,400}?<span class="cm">([^<]*)<\/span>/.exec(indexHtml || '');
    if (ch03) {
      const nums = [...ch03[1].matchAll(/\d+/g)].map((m) => Number(m[0]));
      if (nums.length) {
        const sum = nums.reduce((a, b) => a + b, 0);
        if (sum !== listed) {
          fail('sdks', `CH·03's meta reads ${JSON.stringify(ch03[1].trim())} (sums to ${sum}) but the ledger lists ${listed} packages`);
        }
      }
    }
    counts.sdkPackages = listed;
  }
}

// ══════════════════════════════════════════════════════════════════════════
// Report
// ══════════════════════════════════════════════════════════════════════════
const langList = [...counts.languages].sort();
const summary =
  `checked ${counts.htmlPages} docs page${counts.htmlPages === 1 ? '' : 's'} for chrome ` +
  `(${counts.cssRules} CSS rules: ${counts.railRules} .rail, ${counts.shellRules} .docs-shell), ` +
  `${counts.htmlScanned} HTML pages + ${counts.siteFiles} site files / ${counts.urlOccurrences} absolute URLs for outbound origins, ` +
  `${counts.mdDocs} markdown docs, ` +
  `${counts.fences} code fences (${counts.taggedFences} tagged, ${counts.untaggedFences} untagged) ` +
  `across ${langList.length} languages [${langList.join(' ')}] ` +
  `against ${counts.hljsLanguages} hljs languages, ` +
  `${counts.versionBadges} version badge${counts.versionBadges === 1 ? '' : 's'} against VERSION ${counts.version}`;

process.stdout.write(`check-docs-chrome: root=${root}\n`);
process.stdout.write(`check-docs-chrome: ${summary}\n`);

if (findings.length) {
  process.stderr.write(`\ncheck-docs-chrome: FAIL — ${findings.length} finding${findings.length === 1 ? '' : 's'}:\n`);
  findings.forEach((f, i) => process.stderr.write(`  ${i + 1}. ${f}\n`));
  process.stderr.write('\n');
  process.exit(1);
}
process.stdout.write('check-docs-chrome: PASS\n');

#!/usr/bin/env node
// =============================================================================
// scripts/check-contrast-rendered.mjs — text contrast measured from what the
// compositor produces, not from what the tokens say. Fail-closed.
//
// WHY THIS EXISTS ALONGSIDE check-contrast.mjs
// ────────────────────────────────────────────
// check-contrast.mjs reads the hex values out of the CSS and recomputes the
// ratios. That is a good check and it stays. But it can only see colour that is
// written as colour, and it was green through a nav that measured 3.54:1 on
// screen — because the fade was not in the token. It was `opacity`.
//
// Opacity is invisible to a token reader by construction. `--text-2` can clear
// 4.5:1 against `--ink` in every theme, and then an ancestor with
// `opacity: .55` composites that same text onto the same background at half
// strength, and the reader gets 2.6:1. Nothing in the stylesheet's colour
// values is wrong. The rendered pixels are.
//
// The same hole swallows `rgba()` and any `color-mix()` that resolves to an
// alpha: the token gate sees a colour, the compositor sees a colour AND a
// coverage fraction, and only the second one is what anybody reads.
//
// So this gate does not read the stylesheet at all. It loads the real pages in
// a real browser and, for every element that carries its own text, walks the
// ancestor chain composing:
//
//   - the cumulative opacity of the element and every ancestor, because an
//     ancestor's opacity fades the whole subtree, its own backgrounds included;
//   - each ancestor's background-colour, at its own alpha times that
//     ancestor's cumulative opacity, painted onto what is already below it;
//   - finally the text colour, at its alpha times the element's cumulative
//     opacity, onto that composed backdrop.
//
// Contrast is then computed between the composed text colour and the composed
// backdrop — the two colours a reader's eye actually receives.
//
// WHAT IT ASSERTS
// ───────────────
//   1. Every element with its own text clears WCAG 2.2 AA against its composed
//      backdrop: 4.5:1, or 3:1 where the type is large (>=24px, or >=18.66px
//      at weight 700+). Both themes, both widths, every docs chapter.
//   2. Every colour it meets is one it can parse. A colour function this does
//      not understand is a HARD FAILURE, not a skip — a contrast gate that
//      quietly ignores the colours it cannot read is the exact shape of guard
//      this repo keeps finding.
//   3. The page's own backdrop is opaque. If the root background composes to
//      something translucent then every ratio below it is measured against a
//      guess, and the gate says so instead of reporting numbers.
//
// WHAT IS EXEMPT, AND HOW IT SAYS SO
// ──────────────────────────────────
// Text that is BOTH `aria-hidden="true"` (on itself or an ancestor) AND
// carries no letter or digit — a handful of · and | separators.
//
// Both halves are load-bearing, and this page is why. The landing marks its
// explanatory diagrams aria-hidden: the mux rig, the gate strip, the budget
// lanes. That is 135 elements reading "channel bank · 6 candidates",
// "projected +$1.20 · stays under", "$0 / $5". aria-hidden means "not exposed
// to assistive tech"; it does NOT mean "not visible". A sighted reader reads
// every word of it, and WCAG 1.4.3 is about what is seen — so exempting on the
// attribute alone dropped 135 real strings out of the measurement and called
// the run thorough. Requiring the text to say nothing keeps the exemption to
// what it was meant for.
//
// This is not a waiver list beside the check, which is a thing that rots. It
// is the page stating the claim in the markup, and only for glyphs that say
// nothing. Delete the attribute, or put a word inside it, and the element is
// measured again from the next run.
//
// The exemption is capped anyway (MAX_EXEMPT_FRACTION), and the selftest
// asserts that blanketing real prose in aria-hidden does NOT hide it.
//
// WHY REDUCED MOTION
// ──────────────────
// The landing reveals its bands on scroll: `.rv` starts at `opacity: 0`.
// Measured cold, most of the page "fails" at 1:1 — those elements are not
// faded, they are not shown yet. That is a capture artifact, and the Vulos
// repos have been fooled by that exact artifact before.
//
// Emulating `prefers-reduced-motion: reduce` settles them, because the page
// honours it (`html.js .rv, html.js .rv.in { opacity: 1 }`). It is also the honest thing to
// measure: it is a real setting on real machines, and the words have to be
// legible in it. A side effect worth naming — if that reduced-motion rule ever
// regresses, this gate goes red with cumulative opacity 0 and says so in those
// words, rather than reporting a contrast number for invisible text.
//
// COVERAGE FLOORS
// ───────────────
// Every pass asserts a minimum number of measured elements, and the docs walk
// asserts the chapters were actually DIFFERENT — a hash of each chapter's text
// must be distinct. Walking fourteen slugs and measuring chapter one fourteen
// times is a failure mode this repo has shipped, and a loop counter cannot
// tell the difference.
//
//   node scripts/check-contrast-rendered.mjs            # check what ships
//   node scripts/check-contrast-rendered.mjs --report   # ...and print the worst
//   node scripts/check-contrast-rendered.mjs --selftest # break it, require refusal
//
// EXIT CODES
//   0  every measured element clears AA
//   2  usage
//   3  playwright could not be loaded (never a skip)
//   4  a page failed to load or settle
//   5  a coverage floor was not met
//   6  an element is under its AA threshold
//   7  a colour could not be parsed, or the page backdrop is not opaque
//   8  the docs chapter walk did not visit distinct chapters
//  10  --selftest: a deliberately broken page was NOT refused
// =============================================================================

import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import path from "node:path";

// ── things a copying repo changes ───────────────────────────────────────────
const SITE_DIR = "site";
const THEMES = ["dark", "light"];
const DEFAULT_WIDTHS = [1440, 390];
// 0 means "let the OS pick a free one". A fixed default port is a flake
// waiting for the day two runs overlap, or one is killed with its listener
// still bound — the failure then arrives as an unhandled EADDRINUSE from
// node:net with nothing in it about contrast. --port still pins it if needed.
const DEFAULT_PORT = 0;

// Coverage floors. Set from what the pages measure today, low enough not to be
// brittle and high enough that a parser which stops matching cannot pass.
const MIN_ELEMENTS_LANDING = 700;
const MIN_ELEMENTS_DOCS_CHAPTER = 20;
const MIN_DOCS_CHAPTERS = 14;
const MAX_EXEMPT_FRACTION = 0.15;

const AA_NORMAL = 4.5;
const AA_LARGE = 3.0;

const E_USAGE = 2;
const E_NO_PLAYWRIGHT = 3;
const E_PAGE = 4;
const E_COVERAGE = 5;
const E_CONTRAST = 6;
const E_COLOUR = 7;
const E_CHAPTERS = 8;
const E_SELFTEST = 10;

const tty = process.stderr.isTTY;
const RED = tty ? "\x1b[31m" : "";
const GRN = tty ? "\x1b[32m" : "";
const DIM = tty ? "\x1b[2m" : "";
const BLD = tty ? "\x1b[1m" : "";
const RST = tty ? "\x1b[0m" : "";

function die(code, ...lines) {
  for (const l of lines) process.stderr.write(`${RED}${l}${RST}\n`);
  process.exit(code);
}
function note(...lines) {
  for (const l of lines) process.stderr.write(`${DIM}${l}${RST}\n`);
}

// ── arguments ───────────────────────────────────────────────────────────────
const argv = process.argv.slice(2);
const opts = {
  selftest: false,
  report: false,
  port: DEFAULT_PORT,
  widths: DEFAULT_WIDTHS,
  playwright: process.env.PLAYWRIGHT_DIR || "",
};
function needValue(flag, v) {
  if (v === undefined) die(E_USAGE, `${flag} needs a value`);
  return v;
}
for (let i = 0; i < argv.length; i++) {
  const a = argv[i];
  switch (a) {
    case "--selftest": opts.selftest = true; break;
    case "--report": opts.report = true; break;
    case "--port": opts.port = Number(needValue(a, argv[++i])); break;
    case "--playwright": opts.playwright = needValue(a, argv[++i]); break;
    case "--widths":
      opts.widths = needValue(a, argv[++i]).split(",").map(Number).filter((n) => n > 0);
      break;
    case "-h": case "--help":
      process.stdout.write([
        "check-contrast-rendered.mjs — AA contrast from composited pixels",
        "",
        "  --report               print the worst ratios per pass",
        "  --selftest             break the page on purpose and prove the gate refuses",
        "  --widths A,B           viewport widths (default 1440,390)",
        "  --playwright DIR       where to import playwright from ($PLAYWRIGHT_DIR)",
        "  --port N               static server port (default: an ephemeral one)",
        "",
      ].join("\n"));
      process.exit(0);
      break;
    default: die(E_USAGE, `unknown argument: ${a}`);
  }
}
if (!opts.widths.length) die(E_USAGE, "--widths left no widths to check");

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const siteRoot = path.join(repoRoot, SITE_DIR);

// ── playwright, or nothing ──────────────────────────────────────────────────
async function loadPlaywright() {
  const require = createRequire(import.meta.url);
  const candidates = [];
  if (opts.playwright) candidates.push(opts.playwright);
  candidates.push(
    path.join(repoRoot, "node_modules", "playwright"),
    "playwright",
    "/Users/pc/code/vulos/aql/node_modules/playwright",
    "/Users/pc/code/vulos/vulos-cloud/node_modules/playwright",
  );
  const tried = [];
  for (const c of candidates) {
    try {
      const spec = c.startsWith("/") || c.startsWith(".")
        ? pathToFileURL(require.resolve(c)).href
        : c;
      const mod = await import(spec);
      const chromium = mod.chromium || (mod.default && mod.default.chromium);
      if (chromium) return { chromium, from: c };
      tried.push(`${c} (loaded, but exports no chromium)`);
    } catch (e) {
      tried.push(`${c} (${e.code || e.message})`);
    }
  }
  die(
    E_NO_PLAYWRIGHT,
    "playwright could not be loaded, so NOTHING was measured.",
    "This gate does not skip. A contrast check that reports success without a browser",
    "is worth less than no check, because it looks like one.",
    "Tried:",
    ...tried.map((t) => `  - ${t}`),
    "Fix it with one of:",
    "  npm install --no-save playwright && npx playwright install chromium",
    "  PLAYWRIGHT_DIR=/path/to/node_modules/playwright node scripts/check-contrast-rendered.mjs",
  );
}

// ── a static server for site/, scoped to site/ ──────────────────────────────
const MIME = {
  ".html": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".json": "application/json",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".webp": "image/webp",
  ".woff2": "font/woff2",
  ".md": "text/markdown; charset=utf-8",
  ".txt": "text/plain; charset=utf-8",
};
function serve() {
  return new Promise((resolve) => {
    const server = createServer(async (req, res) => {
      const url = new URL(req.url, "http://localhost");
      let rel = decodeURIComponent(url.pathname);
      if (rel.endsWith("/")) rel += "index.html";
      const file = path.join(siteRoot, rel);
      if (!file.startsWith(siteRoot + path.sep)) {
        res.writeHead(403).end("forbidden");
        return;
      }
      try {
        const body = await readFile(file);
        res.writeHead(200, { "content-type": MIME[path.extname(file)] || "application/octet-stream" }).end(body);
      } catch {
        res.writeHead(404).end("not found");
      }
    });
    server.on("error", (e) => {
      die(E_USAGE, `could not serve ${SITE_DIR}/ on 127.0.0.1:${opts.port}: ${e.code || e.message}`,
        "Pass --port N for a free one, or omit --port to let the OS choose.");
    });
    server.listen(opts.port, "127.0.0.1", () => resolve(server));
  });
}

// ── the measurement, run inside the page ────────────────────────────────────
// Returns { rows, exempt, unparseable, baseOpaque, textHash }. Everything it
// cannot account for it reports; nothing is silently dropped.
const MEASURE = function measure(AA) {
  const parseColour = (s) => {
    if (s === undefined || s === null) return null;
    const t = String(s).trim();
    if (t === "" || t === "transparent" || t === "none") return { r: 0, g: 0, b: 0, a: 0 };
    let m = t.match(/^rgba?\(([^)]+)\)$/);
    if (m) {
      const p = m[1].split(/[,\s/]+/).filter((x) => x !== "").map(Number);
      if (p.length < 3 || p.slice(0, 3).some(Number.isNaN)) return null;
      return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
    }
    m = t.match(/^color\(srgb\s+([^)]+)\)$/);
    if (m) {
      const p = m[1].split(/[\s/]+/).filter((x) => x !== "").map(Number);
      if (p.length < 3 || p.slice(0, 3).some(Number.isNaN)) return null;
      return { r: p[0] * 255, g: p[1] * 255, b: p[2] * 255, a: p.length > 3 ? p[3] : 1 };
    }
    return null; // unknown colour function — the caller must FAIL, not guess
  };
  const over = (f, b) => ({
    r: f.r * f.a + b.r * (1 - f.a),
    g: f.g * f.a + b.g * (1 - f.a),
    b: f.b * f.a + b.b * (1 - f.a),
  });
  const lum = (c) => {
    const ch = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * ch(c.r) + 0.7152 * ch(c.g) + 0.0722 * ch(c.b);
  };
  const ratio = (x, y) => {
    const a = lum(x), b = lum(y);
    const hi = Math.max(a, b), lo = Math.min(a, b);
    return (hi + 0.05) / (lo + 0.05);
  };
  const label = (el) => {
    const cls = typeof el.className === "string" && el.className ? "." + el.className.trim().split(/\s+/).join(".") : "";
    return el.tagName.toLowerCase() + cls;
  };

  const rows = [];
  const unparseable = [];
  let exempt = 0;
  let textAccum = "";

  // Is the page's own ground opaque? Every ratio below is measured onto it.
  const htmlBg = parseColour(getComputedStyle(document.documentElement).backgroundColor);
  const bodyBg = parseColour(getComputedStyle(document.body).backgroundColor);
  const baseOpaque = !!((htmlBg && htmlBg.a === 1) || (bodyBg && bodyBg.a === 1));

  // AA.only scopes the walk to a single element, so the hover resolver can
  // re-measure one node through THIS function rather than a second copy of the
  // compositing maths that could drift from it.
  for (const el of document.querySelectorAll(AA.only || "*")) {
    let text = "";
    for (const n of el.childNodes) if (n.nodeType === 3) text += n.nodeValue;
    text = text.replace(/\s+/g, " ").trim();
    if (!text) continue;

    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") continue;
    const rect = el.getBoundingClientRect();
    if (rect.width < 1 || rect.height < 1) continue;

    textAccum += text + "";

    // Incidental text: aria-hidden AND carrying no letter or digit. BOTH
    // conditions, because aria-hidden alone is the wrong test and it took a
    // sibling repo to show it. llmux marks its explanatory diagrams
    // aria-hidden — 135 elements reading "channel bank · 6 candidates",
    // "projected +$1.20 · stays under", "$0 / $5". aria-hidden means "not
    // exposed to assistive tech", NOT "not visible": a sighted reader reads
    // every word of that, and WCAG 1.4.3 is about what is seen. Exempting on
    // the attribute alone dropped 135 real strings out of the measurement and
    // called the result thorough.
    //
    // Requiring the text to be punctuation-only keeps the exemption to what it
    // was always meant to cover — the · and | separators between items — and
    // nothing that says anything.
    if (el.closest('[aria-hidden="true"]') && !/[\p{L}\p{N}]/u.test(text)) { exempt++; continue; }

    // chain, outermost first, with cumulative opacity at each step
    const chain = [];
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) chain.unshift(n);
    const cum = [];
    let acc = 1;
    for (const n of chain) {
      const o = parseFloat(getComputedStyle(n).opacity);
      acc *= Number.isNaN(o) ? 1 : o;
      cum.push(acc);
    }
    const ownOpacity = cum[cum.length - 1];

    // compose the backdrop under this element's text
    let base = { r: 255, g: 255, b: 255 };
    for (let i = 0; i < chain.length; i++) {
      const raw = getComputedStyle(chain[i]).backgroundColor;
      const bg = parseColour(raw);
      if (!bg) { unparseable.push({ where: label(chain[i]), prop: "background-color", value: String(raw) }); continue; }
      if (bg.a === 0) continue;
      base = over({ r: bg.r, g: bg.g, b: bg.b, a: bg.a * cum[i] }, base);
    }

    const rawFg = cs.color;
    const fgc = parseColour(rawFg);
    if (!fgc) { unparseable.push({ where: label(el), prop: "color", value: String(rawFg) }); continue; }
    const fg = over({ r: fgc.r, g: fgc.g, b: fgc.b, a: fgc.a * ownOpacity }, base);

    const px = parseFloat(cs.fontSize);
    const weight = parseInt(cs.fontWeight, 10) || 400;
    const large = px >= 24 || (weight >= 700 && px >= 18.66);

    rows.push({
      where: label(el),
      text: text.slice(0, 60),
      // Full precision for the comparison, rounded only for display. Several
      // of these land within a hundredth of the floor, and rounding first lets
      // a true 4.497:1 present as "4.5" and pass the >= it just failed.
      ratio: ratio(fg, base),
      need: large ? AA.large : AA.normal,
      opacity: Math.round(ownOpacity * 1000) / 1000,
      px,
      // Tag anything invisible so the driver can hover it. Some affordances
      // are opacity:0 until hover by design (a heading's "#" permalink); the
      // rest is text nobody can see, and the two are only distinguishable by
      // trying it.
      probe: (!AA.only && ownOpacity < 0.05)
        ? (el.setAttribute("data-ccr-probe", String(rows.length)), rows.length)
        : undefined,
    });
  }

  // cheap stable hash of the pass's text, to prove chapters differ
  let h = 5381;
  for (let i = 0; i < textAccum.length; i++) h = ((h * 33) ^ textAccum.charCodeAt(i)) >>> 0;

  return { rows, exempt, unparseable, baseOpaque, textHash: h.toString(16), textLen: textAccum.length };
};

// ── hover-revealed affordances ──────────────────────────────────────────────
// An element at cumulative opacity 0 is one of two very different things: a
// reveal that never fired (a real defect — the reader gets nothing), or an
// affordance that appears on hover, like the "#" permalink beside a docs
// heading. Nothing in the computed style separates them, so the gate tries it:
// hover one representative of each distinct class and see what happens. If it
// appears, it still has to clear AA in that state — a hover affordance is text
// too. If it does not appear, it stays a failure.
//
// One hover per distinct signature per run, cached, because llmux's docs put
// nineteen identical permalinks on every chapter and hovering each of them
// through eighteen chapters and four passes would be 1,368 hovers to learn one
// fact.
async function resolveHidden(page, rows, cache) {
  const hidden = rows.filter((r) => r.probe !== undefined);
  if (!hidden.length) return;
  for (const row of hidden) {
    if (cache.has(row.where)) { Object.assign(row, cache.get(row.where)); continue; }
    let verdict;
    try {
      const el = page.locator(`[data-ccr-probe="${row.probe}"]`);
      await el.hover({ timeout: 2000, force: true });
      await page.waitForTimeout(180);
      const re = await page.evaluate(MEASURE, {
        normal: AA_NORMAL, large: AA_LARGE,
        only: `[data-ccr-probe="${row.probe}"]`,
      });
      verdict = re.rows[0] || null;
      await page.mouse.move(0, 0);
    } catch {
      verdict = null;
    }
    const resolved = verdict && verdict.opacity >= 0.05
      ? { opacity: verdict.opacity, ratio: verdict.ratio, need: verdict.need, hoverRevealed: true }
      : { hoverRevealed: false };
    cache.set(row.where, resolved);
    Object.assign(row, resolved);
  }
}

// ── judging one measurement ─────────────────────────────────────────────────
const failures = [];
const passes = [];
// Verdicts for hover-revealed affordances, keyed by class signature and shared
// across every pass in the run — see resolveHidden().
const hoverCache = new Map();

function judge(where, res, minElements, { collectOnly = false } = {}) {
  const problems = [];

  if (!res.baseOpaque) {
    problems.push({
      kind: "backdrop",
      code: E_COLOUR,
      lines: [
        `${where}: the page's root background is not opaque.`,
        "Every ratio here would be measured against an assumed white canvas, which is a",
        "guess. Give html or body an opaque background, or teach this gate what is behind it.",
      ],
    });
  }
  if (res.unparseable.length) {
    const seen = new Map();
    for (const u of res.unparseable) seen.set(`${u.prop}:${u.value}`, u);
    problems.push({
      kind: "colour",
      code: E_COLOUR,
      lines: [
        `${where}: ${res.unparseable.length} declaration(s) use a colour this gate cannot parse.`,
        "It refuses rather than skipping them: an unread colour is an unmeasured one.",
        ...[...seen.values()].slice(0, 8).map((u) => `  <${u.where}> ${u.prop}: ${u.value}`),
        "Teach parseColour() the syntax.",
      ],
    });
  }
  if (res.rows.length < minElements) {
    problems.push({
      kind: "coverage",
      code: E_COVERAGE,
      lines: [
        `${where}: only ${res.rows.length} text elements were measured, floor is ${minElements}.`,
        "Either the page did not render, or the walk stopped matching. A contrast gate that",
        "examines almost nothing prints PASS just as loudly as one that examines everything.",
      ],
    });
  }
  const measured = res.rows.length + res.exempt;
  if (measured > 0 && res.exempt / measured > MAX_EXEMPT_FRACTION) {
    problems.push({
      kind: "exempt",
      code: E_COVERAGE,
      lines: [
        `${where}: ${res.exempt} of ${measured} text elements are aria-hidden ` +
          `(${(100 * res.exempt / measured).toFixed(1)}%, cap ${100 * MAX_EXEMPT_FRACTION}%).`,
        "aria-hidden exempts a glyph from this gate because it exempts it from meaning.",
        "At this density it is being used to quiet the check instead.",
      ],
    });
  }

  // Still invisible after the hover probe: not a contrast failure, text nobody
  // can see at all.
  const invisible = res.rows.filter((r) => r.opacity < 0.05 && r.hoverRevealed !== true);
  if (invisible.length) {
    problems.push({
      kind: "invisible",
      code: E_CONTRAST,
      lines: [
        `${where}: ${invisible.length} text element(s) render at cumulative opacity < 0.05,`,
        "and hovering them does not bring them back.",
        "Under reduced motion the reveal rule (html.js .rv { opacity: 1 }) should have",
        "settled these; if it has regressed, the words are gone for anyone with that setting.",
        ...invisible.slice(0, 5).map((r) => `  <${r.where}> ${JSON.stringify(r.text)}`),
      ],
    });
  }

  // Hover-revealed affordances are measured in their revealed state — they are
  // text, and a permalink nobody can read is still a permalink nobody can read.
  const under = res.rows
    .filter((r) => (r.opacity >= 0.05 || r.hoverRevealed === true) && r.ratio < r.need)
    .sort((a, b) => a.ratio - b.ratio);
  if (under.length) {
    problems.push({
      kind: "contrast",
      code: E_CONTRAST,
      lines: [
        `${where}: ${under.length} text element(s) under WCAG AA.`,
        ...under.slice(0, 12).map((r) =>
          `  ${r.ratio.toFixed(2).padStart(6)}:1 (need ${r.need}, ${r.px}px, opacity ${r.opacity})  <${r.where}> ${JSON.stringify(r.text)}`),
        ...(under.length > 12 ? [`  ... and ${under.length - 12} more`] : []),
      ],
    });
  }

  if (!collectOnly) {
    if (problems.length) failures.push(...problems);
    else passes.push({ where, n: res.rows.length, exempt: res.exempt, worst: res.rows.reduce((m, r) => Math.min(m, r.ratio / r.need), Infinity) });
    if (opts.report) {
      const worst = [...res.rows].sort((a, b) => a.ratio / a.need - b.ratio / b.need).slice(0, 5);
      note(`${where}: ${res.rows.length} measured, ${res.exempt} aria-hidden`);
      for (const r of worst) note(`    ${r.ratio.toFixed(2).padStart(6)}:1 / ${r.need}  <${r.where}> ${JSON.stringify(r.text.slice(0, 40))}`);
    }
  }
  return problems;
}

// ── driving one pass ────────────────────────────────────────────────────────
async function openPage(browser, url, theme, width, extraCss) {
  const ctx = await browser.newContext({
    viewport: { width, height: 1000 },
    colorScheme: theme,
    reducedMotion: "reduce",
    deviceScaleFactor: 1,
  });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  const resp = await page.goto(url, { waitUntil: "networkidle" });
  if (!resp || !resp.ok()) {
    await ctx.close();
    die(E_PAGE, `${url} did not load (${resp ? resp.status() : "no response"}).`);
  }
  // The page also offers an explicit toggle; set it so the measured theme is
  // the requested one even where a rule keys on [data-theme] rather than the
  // media query. Both paths must agree, and setting both is how we find out.
  await page.evaluate((t) => document.documentElement.setAttribute("data-theme", t), theme);
  if (extraCss) await page.addStyleTag({ content: extraCss });
  await page.waitForTimeout(200);
  if (errors.length) {
    await ctx.close();
    die(E_PAGE, `${url} [${theme}] raised a page error, so the render is not trustworthy:`, ...errors.slice(0, 3));
  }
  return { ctx, page };
}

async function runLanding(browser, base, extraCss, collectOnly) {
  const out = [];
  for (const width of opts.widths) {
    for (const theme of THEMES) {
      const { ctx, page } = await openPage(browser, `${base}/index.html`, theme, width, extraCss);
      const res = await page.evaluate(MEASURE, { normal: AA_NORMAL, large: AA_LARGE });
      await resolveHidden(page, res.rows, hoverCache);
      out.push(...judge(`index.html [${theme} ${width}px]`, res, MIN_ELEMENTS_LANDING, { collectOnly }));
      await ctx.close();
    }
  }
  return out;
}

async function runDocs(browser, base, extraCss, collectOnly) {
  const out = [];
  for (const width of opts.widths) {
    for (const theme of THEMES) {
      const { ctx, page } = await openPage(browser, `${base}/docs.html`, theme, width, extraCss);
      const slugs = await page.evaluate(() => (typeof DOCS !== "undefined" ? DOCS.map((d) => d.slug) : []));
      if (slugs.length < MIN_DOCS_CHAPTERS) {
        out.push({
          kind: "chapters", code: E_CHAPTERS,
          lines: [`docs.html [${theme} ${width}px]: found ${slugs.length} chapters, floor is ${MIN_DOCS_CHAPTERS}.`,
            "The DOCS list is how this gate knows what to walk; if it is empty the walk measures the landing page of the viewer and nothing else."],
        });
        await ctx.close();
        continue;
      }
      const hashes = new Map();
      for (const slug of slugs) {
        await page.evaluate((s) => { location.hash = "#" + s; }, slug);
        // wait for the viewer to actually swap content in
        try {
          await page.waitForFunction(
            (s) => location.hash.replace("#", "") === s && document.getElementById("content")
              && document.getElementById("content").textContent.trim().length > 0,
            slug, { timeout: 5000 },
          );
        } catch {
          out.push({ kind: "chapters", code: E_CHAPTERS,
            lines: [`docs.html [${theme} ${width}px]: chapter '${slug}' never rendered.`] });
          continue;
        }
        await page.waitForTimeout(80);
        const res = await page.evaluate(MEASURE, { normal: AA_NORMAL, large: AA_LARGE });
        await resolveHidden(page, res.rows, hoverCache);
        hashes.set(slug, res.textHash);
        out.push(...judge(`docs.html#${slug} [${theme} ${width}px]`, res, MIN_ELEMENTS_DOCS_CHAPTER, { collectOnly }));
      }
      // The chapters must have been DIFFERENT. Fourteen measurements of chapter
      // one is a green run that proves nothing, and a loop counter cannot tell.
      const distinct = new Set(hashes.values()).size;
      if (distinct < Math.min(MIN_DOCS_CHAPTERS, hashes.size)) {
        out.push({
          kind: "chapters", code: E_CHAPTERS,
          lines: [
            `docs.html [${theme} ${width}px]: walked ${hashes.size} chapters but only ${distinct} distinct texts.`,
            "The walk is re-measuring the same chapter. The hash-routed viewer did not swap",
            "content, so most of the documentation went unmeasured while the run looked thorough.",
          ],
        });
      }
      await ctx.close();
    }
  }
  return out;
}

// ── selftest: prove each refusal actually fires ─────────────────────────────
// Every case is a stylesheet injected into the real page. The first is the
// point of this whole file: an opacity fade leaves every token untouched, so
// check-contrast.mjs stays green through it. If that case ever stops being
// refused here, the gap this gate exists to close is open again.
// `matches` is not documentation. Each case asserts its own selector hits
// something before the measurement is believed. The first draft of this list
// faded `main`, and this landing has no <main> — the mutation was inert, the
// page was unchanged, and "not refused" was the correct answer to a question
// that had not been asked. A mutation that cannot fire tests nothing, and the
// only reason that one was caught is that it was expected to fail.
const SELFTEST_CASES = [
  {
    name: "opacity fade on a text element (invisible to the token gate)",
    css: ".lead { opacity: .30 !important }",
    matches: ".lead",
    expect: "contrast",
  },
  {
    name: "opacity on an ANCESTOR, fading the subtree",
    css: "section { opacity: .35 !important }",
    matches: "section",
    expect: "contrast",
  },
  {
    name: "text colour with a low alpha in rgba()",
    css: "p { color: rgba(255,255,255,.22) !important }",
    matches: "p",
    expect: "contrast",
  },
  {
    name: "a flatly illegible text colour",
    css: "p, li { color: #2f3436 !important }",
    matches: "p, li",
    expect: "contrast",
  },
  {
    name: "a translucent backdrop under the text",
    css: "html, body { background-color: rgba(0,0,0,.4) !important }",
    matches: "body",
    expect: "backdrop",
  },
  {
    name: "a colour function the parser does not know",
    css: "p { color: lab(52% 40 59) !important }",
    matches: "p",
    expect: "colour",
  },
  {
    // The exemption's own abuse case. Every <p> is made illegible AND marked
    // aria-hidden; the gate must still refuse it, because those paragraphs
    // contain words and a sighted reader still has to read them. If this ever
    // stops being refused, aria-hidden has become a way to switch the gate off
    // one element at a time.
    name: "aria-hidden does not exempt real prose from the measurement",
    css: "p { color: #2f3436 !important }",
    attr: true,
    matches: "p",
    expect: "contrast",
  },
];

async function selftest(browser, base) {
  let refused = 0;
  for (const c of SELFTEST_CASES) {
    // Open CLEAN, so the page can be measured before and after the mutation.
    const { ctx, page } = await openPage(browser, `${base}/index.html`, "dark", 1440, null);

    // The mutation must actually land on this page, or the case proves nothing.
    const hit = await page.evaluate((sel) => document.querySelectorAll(sel).length, c.matches);
    if (!hit) {
      await ctx.close();
      die(
        E_SELFTEST,
        `selftest: "${c.name}" targets ${JSON.stringify(c.matches)}, which matches nothing on this page.`,
        "The mutation could not fire, so a 'refused' or 'not refused' verdict would be noise",
        "either way. Point the case at markup that exists.",
      );
    }

    const before = await page.evaluate(MEASURE, { normal: AA_NORMAL, large: AA_LARGE });
    if (c.css) await page.addStyleTag({ content: c.css });
    if (c.attr) {
      await page.evaluate((sel) => {
        for (const el of document.querySelectorAll(sel)) el.setAttribute("aria-hidden", "true");
      }, c.matches);
    }
    await page.waitForTimeout(120);
    const res = await page.evaluate(MEASURE, { normal: AA_NORMAL, large: AA_LARGE });

    // ...and it must actually have CHANGED something. A selector that matches
    // is not a declaration that wins: `p { color: … }` matches all 71
    // paragraphs here and is then out-specified by the rule that already
    // colours them, so the page renders identically and "not refused" is the
    // right answer to a question nobody asked. That is the same inert-mutation
    // trap as a selector matching nothing, one level further in, and it is why
    // this compares the measurement rather than trusting the CSS.
    const sig = (m) => `${m.baseOpaque}|${m.rows.length}|${m.unparseable.length}|` +
      m.rows.map((r) => r.ratio.toFixed(4)).join(",");
    if (sig(before) === sig(res)) {
      await ctx.close();
      die(
        E_SELFTEST,
        `selftest: "${c.name}" changed nothing that this gate can measure.`,
        "The page renders identically with and without it — the declaration is being",
        "out-specified, or the attribute makes no difference here. Either way the case",
        "proves nothing about the gate. Make the mutation bite (a more specific selector,",
        "or !important) rather than accepting the verdict.",
      );
    }
    await resolveHidden(page, res.rows, hoverCache);
    const problems = judge("selftest", res, MIN_ELEMENTS_LANDING, { collectOnly: true });
    await ctx.close();
    const got = problems.map((p) => p.kind);
    if (got.includes(c.expect)) {
      refused++;
      process.stderr.write(`${GRN}  refused${RST} ${c.name} ${DIM}(${c.expect})${RST}\n`);
    } else {
      die(
        E_SELFTEST,
        `selftest: "${c.name}" was NOT refused.`,
        `  expected a '${c.expect}' problem, got: ${got.length ? got.join(", ") : "none"}`,
        "  This gate would pass a page with that defect in it. That is the failure the",
        "  selftest exists to prevent. Do not silence it — repair the check.",
      );
    }
  }

  // The hover resolver has two branches and the landing exercises neither: it
  // has no opacity:0 affordances. The docs page does — every heading carries a
  // "#" permalink at opacity:0 until its heading is hovered — and the clean run
  // passing is already evidence that the "hover brought it back" branch works,
  // because those elements would otherwise be reported as invisible.
  //
  // This is the branch that matters more: hidden text that hovering does NOT
  // bring back. If resolveHidden ever starts waving those through, text that
  // no reader can reach passes as compliant.
  {
    const { ctx, page } = await openPage(browser, `${base}/docs.html`, "dark", 1440,
      ".markdown p, .markdown li { opacity: 0 }");
    const hit = await page.evaluate(() => document.querySelectorAll(".markdown p, .markdown li").length);
    if (!hit) {
      await ctx.close();
      die(E_SELFTEST, "selftest: the docs case matches no .markdown prose, so it proves nothing.");
    }
    const res = await page.evaluate(MEASURE, { normal: AA_NORMAL, large: AA_LARGE });
    await resolveHidden(page, res.rows, new Map()); // fresh cache: no verdict carried in
    const problems = judge("selftest", res, MIN_ELEMENTS_DOCS_CHAPTER, { collectOnly: true });
    await ctx.close();
    if (!problems.some((p) => p.kind === "invisible")) {
      die(
        E_SELFTEST,
        "selftest: prose at opacity:0 that hovering does not reveal was NOT refused.",
        `  got: ${problems.map((p) => p.kind).join(", ") || "no problems at all"}`,
        "  Text nobody can see would pass this gate as compliant.",
      );
    }
    refused++;
    process.stderr.write(`${GRN}  refused${RST} docs prose hidden at opacity:0 that hover never reveals ${DIM}(invisible)${RST}\n`);
  }

  // A gate that fails everything also "refuses" every mutation. The positive
  // control is what separates a working check from a broken one.
  const { ctx, page } = await openPage(browser, `${base}/index.html`, "dark", 1440, null);
  const clean = await page.evaluate(MEASURE, { normal: AA_NORMAL, large: AA_LARGE });
  await resolveHidden(page, clean.rows, hoverCache);
  const cleanProblems = judge("selftest", clean, MIN_ELEMENTS_LANDING, { collectOnly: true });
  await ctx.close();
  if (cleanProblems.length) {
    die(
      E_SELFTEST,
      "selftest: the UNMODIFIED page was refused, so the refusals above prove nothing.",
      ...cleanProblems.flatMap((p) => p.lines).slice(0, 10),
    );
  }
  process.stderr.write(`${GRN}  passed ${RST}the unmodified page ${DIM}(positive control: ${clean.rows.length} elements)${RST}\n`);
  process.stderr.write(`${GRN}${BLD}selftest: ${refused}/${SELFTEST_CASES.length + 1} deliberate breakages refused, clean page accepted${RST}\n`);
}

// ── main ────────────────────────────────────────────────────────────────────
const { chromium, from } = await loadPlaywright();
const server = await serve();
const base = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch();
note(`playwright from ${from}`);

try {
  if (opts.selftest) {
    await selftest(browser, base);
  } else {
    await runLanding(browser, base, null, false);
    await runDocs(browser, base, null, false);

    if (failures.length) {
      process.stderr.write("\n");
      for (const f of failures) for (const l of f.lines) process.stderr.write(`${RED}${l}${RST}\n`);
      const code = failures.map((f) => f.code).sort((a, b) => a - b)[0];
      process.stderr.write(
        `\n${RED}${BLD}check-contrast-rendered: ${failures.length} problem(s) across ${passes.length + failures.length} passes${RST}\n`,
      );
      process.exit(code);
    }
    const totalEls = passes.reduce((n, p) => n + p.n, 0);
    const totalExempt = passes.reduce((n, p) => n + p.exempt, 0);
    process.stderr.write(
      `${GRN}${BLD}check-contrast-rendered: ${totalEls} composited text elements across ` +
      `${passes.length} passes clear WCAG AA${RST}${DIM} (${totalExempt} aria-hidden, exempt)${RST}\n`,
    );
  }
} finally {
  await browser.close();
  server.close();
}

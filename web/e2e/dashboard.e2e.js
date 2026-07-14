/**
 * dashboard.e2e.js — the core flow, in a real browser, against the built bundle:
 * land → navigate to the admin dashboard → read usage → switch tabs.
 *
 * The boot guard proves the app is not blank. This proves it is not INERT — that
 * client-side routing works in the bundle (not just in dev), and that the
 * dashboard actually binds to the gateway API.
 *
 * Defects this file catches:
 *   - Client-side navigation not working in the BUILT bundle (a router basename
 *     mismatch against the "/ui/" base — links 404 or dead-end on the fallback
 *     route, which is invisible in `vite dev` where base is "/").
 *   - The lazy Dashboard chunk failing to load on a client-side transition
 *     (as opposed to a hard load — a different code path, and the one real users
 *     take from the landing page).
 *   - The dashboard rendering but never calling /admin/usage, or dropping the
 *     response (total.requests / total.cost_usd renaming on either side of the
 *     wire silently empties the only numbers the page exists to show).
 *   - Tab switching being dead (each tab is a distinct fetch + table).
 *   - The theme toggle throwing or not persisting (it writes <html data-theme>
 *     and localStorage before first paint — a favourite source of hydration and
 *     "cannot read property of null" crashes).
 */

import { test, expect } from "./fixtures.js";

test("navigates from the landing page to the dashboard and shows live usage", async ({ llmux }) => {
  const { page } = llmux;
  await page.goto("./");

  // Client-side route transition — the path real users take, and the one that
  // exercises the lazy chunk through the router rather than a cold load.
  await page.getByRole("navigation", { name: "Primary" }).getByRole("link", { name: "Dashboard" }).click();

  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
  await expect(page).toHaveURL(/\/ui\/app$/);

  // Health banner is bound to the mocked /health.
  await expect(page.locator(".banner.ok")).toContainText("gateway online");
  await expect(page.locator(".banner.ok")).toContainText("openai");

  // Usage cards are bound to the mocked /admin/usage. These exact numbers come
  // from the fixture, so a stale/blank render cannot pass.
  const cards = page.locator(".cards .card");
  await expect(cards.filter({ hasText: "Requests" })).toContainText("1,240");
  await expect(cards.filter({ hasText: "Cost" })).toContainText("$12.50");

  // Per-model breakdown table actually has the mocked rows.
  const rows = page.locator(".table tbody tr");
  await expect(rows).toHaveCount(2);
  await expect(rows.first()).toContainText("gpt-4o");
});

test("switches dashboard tabs and loads each view's data", async ({ llmux }) => {
  const { page } = llmux;
  await page.goto("./app");
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();

  // keys tab → GET /admin/keys
  const keysCall = page.waitForResponse((r) => r.url().endsWith("/admin/keys"));
  await page.getByRole("tab", { name: "keys" }).click();
  await keysCall;

  await expect(page.getByRole("tab", { name: "keys" })).toHaveAttribute("aria-selected", "true");
  await expect(page.locator(".section-eyebrow")).toContainText("2 keys");
  await expect(page.locator(".table tbody tr").first()).toContainText("prod");
  // The tab is reflected in the URL hash (deep-linkable).
  await expect(page).toHaveURL(/#keys$/);

  // models tab → GET /v1/models
  const modelsCall = page.waitForResponse((r) => r.url().endsWith("/v1/models"));
  await page.getByRole("tab", { name: "models" }).click();
  await modelsCall;

  await expect(page.locator(".section-eyebrow")).toContainText("2 models in catalog");
  const modelRows = page.locator(".table tbody tr");
  await expect(modelRows).toHaveCount(2);
  await expect(modelRows.first()).toContainText("gpt-4o");

  // The models filter is a real interaction, not decoration.
  await page.getByLabel("Filter models").fill("claude");
  await expect(page.locator(".table tbody tr")).toHaveCount(1);
  await expect(page.locator(".table tbody tr")).toContainText("claude-sonnet");
});

test("theme toggle cycles and persists", async ({ llmux }) => {
  const { page } = llmux;
  await page.goto("./");

  const html = page.locator("html");
  const toggle = page.getByRole("button", { name: /^Theme:/ });

  // Default is "system": no data-theme attribute, so prefers-color-scheme drives
  // the palette (see index.html's pre-paint script + styles.css).
  await expect(html).not.toHaveAttribute("data-theme", /.+/);

  // system → light → dark. Asserting the attribute (not a class) is the contract
  // the pre-paint inline script in index.html depends on; if the app and that
  // script ever disagree, users get a flash of the wrong theme on every load.
  await toggle.click();
  await expect(html).toHaveAttribute("data-theme", "light");
  await toggle.click();
  await expect(html).toHaveAttribute("data-theme", "dark");

  // Persisted across a reload by the pre-paint script — the whole point of it.
  await page.reload();
  await expect(html).toHaveAttribute("data-theme", "dark");
});

test("docs renders a chapter and switches between them", async ({ llmux }) => {
  const { page } = llmux;
  await page.goto("./docs");

  // The docs chunk statically imports the markdown (?raw) AND 18 highlight.js
  // language modules. If any of that fails to resolve at build time, this route
  // is the only place it shows — as a blank page or a stuck Suspense fallback.
  await expect(page.getByRole("heading", { name: /quickstart/i }).first()).toBeVisible();

  // Markdown actually rendered to HTML (not dumped as raw text): the ?raw
  // imports resolved and react-markdown ran.
  const code = page.locator("article.markdown .codeblock pre code").first();
  await expect(code).toBeVisible();

  // ...and it was SYNTAX-HIGHLIGHTED. `hljs` classes only appear if
  // rehype-highlight and the registered highlight.js language modules really
  // executed. This is the assertion that guards the phantom-dependency fix:
  // highlight.js was undeclared and resolved only through npm's hoisting of
  // rehype-highlight → lowlight → highlight.js. If that hoist ever stops (a
  // dependency bump, a different installer), these 18 static imports break and
  // /docs is the ONLY route where it shows.
  await expect(code).toHaveClass(/hljs/);
  await expect(code.locator("span.hljs-built_in, span.hljs-string, span.hljs-comment, span.hljs-keyword").first()).toBeVisible();

  // Switching chapters is a real route change. Scope to the sidebar: the
  // "next chapter" footer link also reads "Providers".
  await page.locator("nav.docs-side").getByRole("link", { name: /^Providers$/i }).click();
  await expect(page).toHaveURL(/\/docs\/providers$/);
  await expect(page.locator("nav.docs-side a.active")).toHaveText(/Providers/i);
  await expect(page.getByRole("heading", { name: /providers/i }).first()).toBeVisible();
});

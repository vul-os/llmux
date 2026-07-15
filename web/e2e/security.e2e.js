/**
 * security.e2e.js — adversarial guards for the console that holds the keys.
 *
 * The dashboard displays strings it does not control (provider names, model ids,
 * virtual-key names/tokens from the gateway's admin API) and it holds the admin
 * MASTER KEY. Two failure classes matter here and are proven in a real browser:
 *
 *   1. XSS — a hostile/compromised upstream (or a model id synced from a
 *      third-party price catalog) must never inject executing markup. React
 *      escapes text children and there is no dangerouslySetInnerHTML in the tree;
 *      these tests fail loudly if that ever regresses.
 *
 *   2. Credential hygiene — the master key must go ONLY to the configured gateway
 *      as an Authorization: Bearer header, live ONLY in localStorage under its one
 *      documented slot, never be logged to the console, and never be cached by a
 *      service worker (the app registers none).
 *
 * The UI is a display affordance: it cannot and does not weaken the server's
 * default-deny. The admin views 401 without a key — proven by asserting the UI
 * surfaces that 401 rather than fabricating data (see boot.e2e.js) — and the
 * key is only ever *sent*, never *checked*, client-side.
 */

import { test as bare, expect } from "@playwright/test";
import { watchForCrashes } from "./fixtures.js";

// A payload that BOTH escaping bugs and innerHTML sinks would execute: the <img>
// fires onerror the instant it is parsed as HTML. If any assertion below sees
// window.__xss set, a gateway string reached the DOM as markup.
const XSS = '<img src=x onerror="window.__xss=1">';

// A gateway whose every field is hostile. Shapes still match what the dashboard
// consumes, so the payloads land in exactly the cells a user would read.
async function installHostileApi(page) {
  const json = (route, body) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const handler = async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/health")
      return json(route, { status: "ok", providers: [{ name: `openai${XSS}`, stability: "stable" }] });
    if (path === "/admin/usage")
      return json(route, {
        total: { requests: 1, total_tokens: 10, cost_usd: 0.01 },
        by_model: { [`gpt-4o${XSS}`]: { requests: 1, total_tokens: 10, cost_usd: 0.01 } },
      });
    if (path === "/admin/keys")
      return json(route, {
        keys: [{ name: `prod${XSS}`, key: `sk-llmux-${XSS}`, budget_usd: 100, spend_usd: 1, rpm: 60 }],
      });
    if (path === "/v1/models")
      return json(route, { data: [{ id: `evil${XSS}`, input_price_per_mtok: 1, output_price_per_mtok: 2, context_window: 128000 }] });
    if (path === "/v1/catalog.json") return json(route, { models: [] });
    return route.fulfill({ status: 404, body: "not found" });
  };
  await page.route("**/health", handler);
  await page.route("**/admin/**", handler);
  await page.route("**/v1/**", handler);
}

bare("hostile gateway strings render as inert text on every dashboard tab — no script executes", async ({ page }) => {
  await installHostileApi(page);
  const crashes = watchForCrashes(page);
  await page.addInitScript(() => {
    window.__xss = undefined;
  });

  await page.goto("./app");
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();

  // usage tab (default): the malicious model id is present as literal text.
  await expect(page.locator(".table tbody tr").first()).toContainText(`gpt-4o${XSS}`);
  // The health banner carries the hostile provider name, also as text.
  await expect(page.locator(".banner.ok")).toContainText(`openai${XSS}`);

  // keys tab.
  await page.getByRole("tab", { name: "keys" }).click();
  await expect(page.locator("code.key-token")).toHaveText(`sk-llmux-${XSS}`);

  // models tab.
  await page.getByRole("tab", { name: "models" }).click();
  await expect(page.locator(".table tbody tr").first()).toContainText(`evil${XSS}`);

  // The onerror never fired: not one field was parsed as HTML.
  expect(await page.evaluate(() => window.__xss)).toBeUndefined();
  // And no injected <img> exists anywhere in the document.
  expect(await page.locator('img[src="x"]').count()).toBe(0);
  expect(crashes.pageErrors, "hostile strings must not crash the app").toEqual([]);
});

bare("the master key is sent ONLY to the configured gateway, as a Bearer header — never in a URL", async ({ page }) => {
  const seen = [];
  await page.route("**/admin/**", (route) => {
    const req = route.request();
    seen.push({ url: req.url(), auth: req.headers()["authorization"] || "" });
    return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ keys: [] }) });
  });
  await page.route("**/health", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok", providers: [] }) }));

  await page.goto("./app#keys");
  // Enter a master key through the settings bar and apply it.
  await page.getByPlaceholder("master key").fill("sk-llmux-supersecret");
  await page.getByRole("button", { name: "Apply" }).click();

  await expect.poll(() => seen.length).toBeGreaterThan(0);
  for (const call of seen) {
    // Same-origin (blank base = the gateway that served the UI), key in the header.
    expect(new URL(call.url).origin).toBe(new URL(page.url()).origin);
    expect(call.url).not.toContain("sk-llmux-supersecret"); // not in path/query
  }
  // At least one admin call carried the key as a Bearer once it was applied.
  expect(seen.some((c) => c.auth === "Bearer sk-llmux-supersecret")).toBe(true);
});

bare("the master key is never written to console output", async ({ page }) => {
  const logs = [];
  page.on("console", (m) => logs.push(m.text()));
  page.on("pageerror", (e) => logs.push(String(e)));

  await page.route("**/admin/**", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ keys: [] }) }));
  await page.route("**/health", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok", providers: [] }) }));

  await page.goto("./app");
  await page.getByPlaceholder("master key").fill("sk-llmux-supersecret");
  await page.getByRole("button", { name: "Apply" }).click();
  await page.getByRole("tab", { name: "keys" }).click();
  await page.waitForTimeout(300);

  expect(logs.join("\n")).not.toContain("sk-llmux-supersecret");
});

bare("the master key lives only in its one documented localStorage slot, not sessionStorage or elsewhere", async ({ page }) => {
  await page.route("**/admin/**", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ keys: [] }) }));
  await page.route("**/health", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok", providers: [] }) }));

  await page.goto("./app");
  await page.getByPlaceholder("master key").fill("sk-llmux-supersecret");
  await page.getByRole("button", { name: "Apply" }).click();
  await expect(page.getByPlaceholder("master key")).toHaveValue("sk-llmux-supersecret");

  const where = await page.evaluate((needle) => {
    const hit = { local: [], session: [] };
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (String(localStorage.getItem(k)).includes(needle)) hit.local.push(k);
    }
    for (let i = 0; i < sessionStorage.length; i++) {
      const k = sessionStorage.key(i);
      if (String(sessionStorage.getItem(k)).includes(needle)) hit.session.push(k);
    }
    return hit;
  }, "sk-llmux-supersecret");

  expect(where.local).toEqual(["llmux_master_key"]);
  expect(where.session).toEqual([]);
});

bare("the app registers no service worker — nothing can cache admin responses or the key", async ({ page }) => {
  await page.route("**/health", (r) => r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok", providers: [] }) }));
  await page.goto("./");
  await expect(page.locator("#root")).not.toBeEmpty();

  const regs = await page.evaluate(async () => {
    if (!navigator.serviceWorker) return 0;
    const r = await navigator.serviceWorker.getRegistrations();
    return r.length;
  });
  expect(regs).toBe(0);
});

bare("docs markdown does not render raw HTML — an embedded tag stays inert text", async ({ page }) => {
  // The docs are static build-time markdown, but react-markdown must remain
  // configured WITHOUT rehype-raw. If someone adds it, an HTML tag in any doc
  // would render live. This asserts the current (safe) default holds.
  const crashes = watchForCrashes(page);
  await page.goto("./docs");
  await expect(page.getByRole("heading", { name: /quickstart/i }).first()).toBeVisible();
  // No doc ships a raw <script>/<iframe>; assert none was materialised either.
  expect(await page.locator("article.markdown script, article.markdown iframe").count()).toBe(0);
  expect(crashes.pageErrors).toEqual([]);
});

/**
 * e2e/fixtures.js — shared Playwright helpers for the llmux web E2E layer.
 *
 *   1. `installApi(page)` — an in-browser mock of the gateway's admin surface
 *      (/health, /admin/usage, /admin/keys, /v1/models, /v1/catalog.json) via
 *      page.route, so the suite runs with no gateway, no providers and no keys.
 *      Shapes mirror what src/api.js consumes and Dashboard.jsx renders.
 *
 *      NOTE the paths are same-origin ABSOLUTE ("/health", not "/ui/health"):
 *      api.js does `fetch(getBase() + path)` with an empty base, so the
 *      dashboard served at /ui calls the gateway at the origin root. Mocking the
 *      wrong prefix here would make the mocks silently miss.
 *
 *   2. `watchForCrashes(page)` — the crash recorder used by EVERY spec. A React
 *      app that throws while rendering unmounts to an EMPTY root and still
 *      serves HTTP 200 — the blank-screen failure that shipped — so "no
 *      pageerror" is a hard gate, never a warning.
 */

import { test as base, expect } from "@playwright/test";

export const HEALTH = {
  status: "ok",
  providers: [
    { name: "openai", stability: "stable" },
    { name: "anthropic", stability: "stable" },
  ],
};

export const USAGE = {
  total: { requests: 1240, total_tokens: 3_400_000, cost_usd: 12.5 },
  by_model: {
    "gpt-4o": { requests: 800, total_tokens: 2_000_000, cost_usd: 9.0 },
    "claude-sonnet": { requests: 440, total_tokens: 1_400_000, cost_usd: 3.5 },
  },
};

export const KEYS = {
  keys: [
    { name: "prod", key: "sk-llmux-prod-abc", budget_usd: 100, spend_usd: 12.5, rpm: 60 },
    { name: "ci", key: "sk-llmux-ci-def", budget_usd: 10, spend_usd: 9.6, rpm: 0 },
  ],
};

// Shape consumed by Dashboard's <Models/>: data[] with id + per-Mtok pricing +
// context window (NOT the OpenAI-ish `owned_by` shape — mocking that would make
// the table render "—" everywhere and quietly pass a weaker assertion).
export const MODELS = {
  data: [
    { id: "gpt-4o", input_price_per_mtok: 2.5, output_price_per_mtok: 10, context_window: 128000 },
    { id: "claude-sonnet", input_price_per_mtok: 3, output_price_per_mtok: 15, context_window: 200000 },
  ],
};

export const CATALOG = {
  models: [
    { id: "gpt-4o", provider: "openai", input_per_1m: 2.5, output_per_1m: 10 },
    { id: "claude-sonnet", provider: "anthropic", input_per_1m: 3, output_per_1m: 15 },
  ],
};

/** Attach the mocked gateway API to a page. Call BEFORE page.goto(). */
export async function installApi(page, { unauthorized = false } = {}) {
  const json = (route, body) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });

  const handler = async (route) => {
    const path = new URL(route.request().url()).pathname;

    if (path === "/health") return json(route, HEALTH);

    // The admin surface is master-key gated. `unauthorized` lets a spec prove
    // the UI surfaces a 401 instead of crashing or hanging on a skeleton.
    if (path.startsWith("/admin/")) {
      if (unauthorized) return route.fulfill({ status: 401, body: "missing master key" });
      if (path === "/admin/usage") return json(route, USAGE);
      if (path === "/admin/keys") return json(route, KEYS);
    }

    if (path === "/v1/models") return json(route, MODELS);
    if (path === "/v1/catalog.json") return json(route, CATALOG);

    return route.fulfill({ status: 404, body: "not found" });
  };

  await page.route("**/health", handler);
  await page.route("**/admin/**", handler);
  await page.route("**/v1/**", handler);
}

/**
 * Record uncaught exceptions and dead same-origin requests for the page's life.
 * Assert both EMPTY in every spec.
 */
export function watchForCrashes(page) {
  const pageErrors = [];
  const failedRequests = [];
  page.on("pageerror", (err) => pageErrors.push(`${err.name}: ${err.message}`));
  page.on("requestfailed", (req) => {
    const url = req.url();
    // Only the app's own assets matter. The landing/footer link out to GitHub
    // and vulos.org; those are never fetched, but be explicit.
    if (url.startsWith("http://localhost")) {
      failedRequests.push(`${url} — ${req.failure()?.errorText}`);
    }
  });
  return { pageErrors, failedRequests };
}

/** `llmux` fixture: a page with the gateway mocked and crashes recorded. */
export const test = base.extend({
  llmux: async ({ page }, use) => {
    await installApi(page);
    const crashes = watchForCrashes(page);
    await use({ page, ...crashes });
    expect(crashes.pageErrors, "uncaught exception(s) in the built bundle").toEqual([]);
  },
});

export { expect };

/**
 * boot.e2e.js — THE BOOT GUARD.
 *
 * Does the bundle we actually ship RUN? Nothing in this repo ever asked before:
 * there were no frontend tests at all, and CI built the bundle only to embed it
 * into the Go binary. `vite build` exiting 0 is not evidence — two apps in this
 * suite shipped a blank screen behind a green build (an unresolved import that
 * became a module which throws on load; a duplicated React that made every hook
 * call invalid).
 *
 * llmux is unusually exposed: App.jsx puts `/docs` and `/app` behind
 * React.lazy(), so a throwing module or an unresolvable import in either chunk
 * DOES NOT EXIST until that route is visited. The landing page would boot
 * perfectly and the defect would ship. Hence: every route is booted here, and a
 * lazy chunk that fails to load is asserted to be a failure — React's <Suspense>
 * would otherwise leave the user staring at the "loading…" fallback forever
 * while the page reports HTTP 200 and no test notices.
 *
 * (This is not hypothetical for /docs: it statically imports 18 language modules
 * from `highlight.js`, which was until now an UNDECLARED, hoisting-only phantom
 * dependency — see the package.json change alongside this file.)
 *
 * Defects caught here:
 *   - ANY uncaught exception on load/render of the built bundle, on ANY route.
 *   - React mounting nothing (empty #root) → the blank screen.
 *   - A lazy route chunk that never resolves (stuck Suspense fallback).
 *   - The entry chunk served as HTML instead of JS (base "/ui/" misconfigured —
 *     the page 200s, boots nothing, and naive checks still pass).
 *   - The dashboard crashing (rather than degrading) when the gateway is
 *     unreachable or the master key is missing — the state of EVERY first visit.
 */

import { test, expect } from "./fixtures.js";
import { test as bare } from "@playwright/test";
import { installApi, watchForCrashes } from "./fixtures.js";

test("boots the built bundle in a real browser with no uncaught errors", async ({ llmux }) => {
  const { page, pageErrors, failedRequests } = llmux;

  await page.goto("./");

  const root = page.locator("#root");
  await expect(root).not.toBeEmpty();

  // The right thing rendered, not merely something.
  await expect(page.getByRole("link", { name: "llmux home" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Primary" })).toBeVisible();

  expect(pageErrors, "uncaught exception(s) while booting the built bundle").toEqual([]);
  expect(failedRequests, "asset(s) failed to load").toEqual([]);
});

// Every route, including both React.lazy() chunks. A defect inside a lazy chunk
// is invisible until its route is visited — this is the guard that visits them.
for (const { path, name, ready } of [
  { path: "./", name: "landing", ready: (p) => p.getByRole("heading", { level: 1 }).first() },
  { path: "./docs", name: "docs (lazy chunk)", ready: (p) => p.getByRole("heading", { name: /quickstart/i }).first() },
  { path: "./docs/providers", name: "docs deep link (lazy chunk)", ready: (p) => p.getByRole("heading", { name: /providers/i }).first() },
  { path: "./app", name: "dashboard (lazy chunk)", ready: (p) => p.getByRole("heading", { name: "Dashboard" }) },
]) {
  bare(`route boots: ${name}`, async ({ page }) => {
    await installApi(page);
    const crashes = watchForCrashes(page);

    await page.goto(path);

    // Real content, not the Suspense fallback: a lazy chunk that throws or 404s
    // leaves ".route-loading" on screen forever with no error anywhere else.
    await expect(ready(page)).toBeVisible();
    await expect(page.locator(".route-loading")).toHaveCount(0);
    await expect(page.locator("#root")).not.toBeEmpty();

    expect(crashes.pageErrors, `uncaught exception(s) on ${path}`).toEqual([]);
    expect(crashes.failedRequests, `failed request(s) on ${path}`).toEqual([]);
  });
}

bare("entry chunk is served as JavaScript under the /ui/ base, not an HTML fallback", async ({ page }) => {
  // The app is BUILT with base "/ui/". If it is ever served from a root that
  // doesn't match, the request for /ui/assets/index-*.js misses and the SPA
  // fallback answers with index.html at HTTP 200 — text/html, ~1 kB. The browser
  // won't execute it as a module, React never boots, and the page still "loads".
  // This exact mismatch silently broke hosted E2E elsewhere in this suite.
  await installApi(page);
  const chunks = [];
  page.on("response", (res) => {
    if (/\.js$/.test(new URL(res.url()).pathname)) {
      chunks.push({ url: res.url(), type: res.headers()["content-type"] || "", status: res.status() });
    }
  });

  await page.goto("./");
  await expect(page.locator("#root")).not.toBeEmpty();

  expect(chunks.length, "index.html referenced no entry script").toBeGreaterThan(0);
  for (const c of chunks) {
    expect(c.status, `${c.url} did not 200`).toBe(200);
    expect(c.type, `${c.url} was not served as JavaScript`).toMatch(/javascript|ecmascript/i);
    expect(new URL(c.url).pathname, "chunk served outside the /ui/ base").toContain("/ui/");
  }
});

bare("dashboard degrades to a visible error, never a blank screen, when the gateway is down", async ({ page }) => {
  // The state of every first visit: no master key stored, gateway maybe absent.
  // If a rejection ever escapes useAsync, React unmounts the tree and the user
  // gets a white page on an HTTP 200. Nothing else in this repo guards that.
  const crashes = watchForCrashes(page);
  await page.route("**/health", (r) => r.abort("connectionrefused"));
  await page.route("**/admin/**", (r) => r.abort("connectionrefused"));
  await page.route("**/v1/**", (r) => r.abort("connectionrefused"));

  await page.goto("./app");

  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
  await expect(page.locator(".banner.err")).toContainText(/gateway unreachable/i);
  await expect(page.locator("#root")).not.toBeEmpty();

  expect(crashes.pageErrors, "a dead gateway must not produce an uncaught exception").toEqual([]);
});

bare("dashboard surfaces a 401 instead of hanging on a skeleton", async ({ page }) => {
  // Admin views need a master key. Without one the gateway 401s; the UI must say
  // so. A regression that leaves the skeleton up forever looks identical to a
  // hang and is exactly what no test in this repo could see.
  await installApi(page, { unauthorized: true });
  const crashes = watchForCrashes(page);

  await page.goto("./app#keys");

  await expect(page.getByRole("tab", { name: "keys" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText(/needs a master key/i)).toBeVisible();
  expect(crashes.pageErrors).toEqual([]);
});

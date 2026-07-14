/**
 * Playwright E2E config — llmux web (landing, docs, admin dashboard).
 *
 * WHY THIS EXISTS: `vite build` exiting 0 proves nothing about whether the
 * bundle it emitted actually RUNS. Two apps in this suite shipped a blank
 * screen behind a green build (an unresolved import the bundler turned into a
 * module that throws on load; a duplicated React that broke every hook call).
 * Nothing here had ever loaded the BUILT bundle in a browser. This suite does.
 *
 * llmux is *especially* exposed to that failure mode: App.jsx code-splits
 * `/docs` and `/app` behind React.lazy(). A module-level throw or an
 * unresolvable import inside either chunk does not exist until the route is
 * visited — the landing page boots fine and the defect ships. So the boot guard
 * here walks EVERY route, not just `/`.
 *
 * The suite drives the PRODUCTION build through `vite preview`, which serves it
 * under the configured base ("/ui/", matching how the Go gateway embeds and
 * mounts it — see web/embed.go). The admin API is mocked in-browser with
 * page.route, so the run is hermetic: no gateway, no providers, no keys.
 *
 * Prereqs:  npm run build            (`pretest:e2e` does it)
 *           npx playwright install chromium
 * Run:      npm test
 */

import { defineConfig, devices } from "@playwright/test";

// Uncommon port: a stale preview of another Vulos app on 5173/4173 must never
// be mistaken for llmux. Override with E2E_PORT.
const PORT = process.env.E2E_PORT ? Number(process.env.E2E_PORT) : 47341;

// The app is BUILT with base "/ui/" (vite.config.js), so index.html references
// /ui/assets/*. `vite preview` reads the same config and serves under /ui/ —
// baseURL must include it or every navigation would hit the SPA fallback and we
// would be testing a 404 page. (This exact mismatch has silently broken hosted
// E2E elsewhere in the suite.)
const BASE_URL = `http://localhost:${PORT}/ui/`;

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.e2e.js",
  timeout: 30_000,
  expect: { timeout: 7_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  use: {
    baseURL: BASE_URL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    serviceWorkers: "block",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `npx vite preview --port ${PORT} --strictPort`,
    url: BASE_URL,
    // Never reuse a server on this port — it could be a different app, and we
    // would silently test the wrong bundle.
    reuseExistingServer: false,
    timeout: 60_000,
  },
});

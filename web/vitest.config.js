import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// Unit / component layer (jsdom). This is DISTINCT from the Playwright E2E layer
// (playwright.config.js), which drives the real production bundle in Chromium.
// Split of duties, on purpose:
//   - vitest here  → fast, hermetic assertions on pure logic (api.js theme +
//     credential storage) and React-escaping of gateway-controlled strings, at
//     the component boundary where a regression is cheapest to localise.
//   - Playwright   → does the bundle actually RUN, route, and stay quiet on
//     `pageerror`, in a real browser.
// Neither replaces the other; the E2E `serviceWorkers: "block"` guard, base-path
// checks, and lazy-chunk boot proofs cannot be expressed in jsdom.
export default defineConfig({
  plugins: [react()],
  // The app relies on the automatic JSX runtime (no `import React` in components);
  // pin esbuild to it so test files transform the same way the app bundle does.
  esbuild: { jsx: "automatic", jsxImportSource: "react" },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.js"],
    include: ["src/**/*.test.{js,jsx}"],
    // Playwright specs live in e2e/ and must never be collected by vitest — they
    // import @playwright/test, which throws outside the Playwright runner.
    exclude: ["e2e/**", "node_modules/**", "dist/**"],
    css: false,
    // clearMocks resets call history between tests. NOT restoreMocks — it would
    // strip the implementations off the vi.mock("../api.js") module factory.
    clearMocks: true,
    unstubGlobals: true,
  },
});

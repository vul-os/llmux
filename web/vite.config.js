import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { licensesTxt } from "./vite-plugin-licenses.js";

// Built with base "/ui/" so the bundle can be embedded into the gateway binary
// and served at /ui. For the standalone llmux.to deploy, rebuild with base "/".
export default defineConfig({
  base: "/ui/",
  plugins: [react(), licensesTxt()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // Keep upstream @license / @preserve banners in the bundled JS. MIT,
        // BSD and ISC all require the copyright notice to travel with every
        // copy, and this bundle is embedded and served to browsers. Vite 8
        // bundles JS with rolldown, whose minifier drops every comment unless
        // this is set. (esbuild.legalComments does NOT do it under Vite 8 — it
        // only reaches the CSS pipeline.)
        comments: { legal: true },
      },
    },
  },
});

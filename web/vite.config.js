import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Built with base "/ui/" so the bundle can be embedded into the gateway binary
// and served at /ui. For the standalone llmux.to deploy, rebuild with base "/".
export default defineConfig({
  base: "/ui/",
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true },
});

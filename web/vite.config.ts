import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import type { Plugin } from "vite";
import { defineConfig } from "vitest/config";

// web/embed.go embeds dist/, so dist/.gitkeep must survive the build's emptyOutDir.
function keepEmbedPlaceholder(): Plugin {
  return {
    name: "menagerie-keep-embed-placeholder",
    apply: "build",
    closeBundle() {
      writeFileSync(fileURLToPath(new URL("./dist/.gitkeep", import.meta.url)), "");
    },
  };
}

export default defineConfig({
  plugins: [react(), keepEmbedPlaceholder()],
  build: { outDir: "dist", emptyOutDir: true, sourcemap: true },
  server: {
    proxy: {
      "/api": { target: process.env.MENAGERIE_URL ?? "http://127.0.0.1:4318", changeOrigin: false },
      "/auth": {
        target: process.env.MENAGERIE_URL ?? "http://127.0.0.1:4318",
        changeOrigin: false,
      },
    },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}", "test/contract/**/*.test.ts"],
    setupFiles: ["./src/test-setup.ts"],
    restoreMocks: true,
  },
});

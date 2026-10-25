import { readFileSync } from "node:fs";
import { defineConfig, devices } from "@playwright/test";

// docs/demo/record.sh records the demo against a fresh acceptance fixture.
const infoPath = process.env.MENAGERIE_BROWSER_INFO;
if (!infoPath) throw new Error("MENAGERIE_BROWSER_INFO is required: run docs/demo/record.sh");
const info = JSON.parse(readFileSync(infoPath, "utf8")) as { site: string; mode?: string };
if (info.mode !== "image") throw new Error(`${infoPath} does not describe the image run`);
const size = { width: 1280, height: 800 };

export default defineConfig({
  testDir: "test/integrated",
  testMatch: "demo.spec.ts",
  workers: 1,
  reporter: [["list"]],
  outputDir: process.env.MENAGERIE_DEMO_OUTPUT ?? "evidence/demo",
  timeout: 15 * 60_000,
  expect: { timeout: 5 * 60_000 },
  use: {
    baseURL: info.site,
    ...devices["Desktop Chrome"],
    viewport: size,
    video: { mode: "on", size },
  },
});

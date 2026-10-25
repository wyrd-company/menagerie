import { readFileSync } from "node:fs";
import { defineConfig, devices } from "@playwright/test";

// The acceptance run drives the built image that
// web/test/integrated/acceptance.sh starts: the integrated story first, then
// the failures and the restart on the workspaces it left. Every test keeps
// its video and trace under MENAGERIE_EVIDENCE.
const infoPath = process.env.MENAGERIE_BROWSER_INFO;
if (!infoPath)
  throw new Error(
    "MENAGERIE_BROWSER_INFO is required: start the fixture with acceptance.sh fixture",
  );
const info = JSON.parse(readFileSync(infoPath, "utf8")) as { site: string; mode?: string };
if (info.mode !== "image") throw new Error(`${infoPath} does not describe the image run`);
const evidence = process.env.MENAGERIE_EVIDENCE ?? "evidence/acceptance";

export default defineConfig({
  testDir: "test/integrated",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"], ["json", { outputFile: `${evidence}/playwright-report.json` }]],
  outputDir: `${evidence}/playwright`,
  // Test-harness bounds for real image pulls, builds and containers; the
  // product applies no timeout to its jobs.
  timeout: 15 * 60_000,
  expect: { timeout: 5 * 60_000 },
  use: {
    baseURL: info.site,
    trace: "on",
    video: "on",
    ...devices["Desktop Chrome"],
    viewport: { width: 1360, height: 900 },
  },
  projects: [
    { name: "story", testMatch: "integrated.spec.ts" },
    { name: "acceptance", testMatch: "acceptance.spec.ts", dependencies: ["story"] },
  ],
});

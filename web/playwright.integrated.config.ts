import { readFileSync } from "node:fs";
import { defineConfig, devices } from "@playwright/test";

// The integrated run drives the built UI served by the real workspace
// service that web/test/integrated/run.sh starts; MENAGERIE_BROWSER_INFO
// names the fixture's description of it.
const infoPath = process.env.MENAGERIE_BROWSER_INFO;
if (!infoPath)
  throw new Error("MENAGERIE_BROWSER_INFO is required: start the fixture with run.sh up");
const info = JSON.parse(readFileSync(infoPath, "utf8")) as { site: string };

export default defineConfig({
  testDir: "test/integrated",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  // Test-harness bounds for real image pulls, builds and containers; the
  // product applies no timeout to its jobs.
  timeout: 15 * 60_000,
  expect: { timeout: 5 * 60_000 },
  use: {
    baseURL: info.site,
    trace: "retain-on-failure",
    ...devices["Desktop Chrome"],
    viewport: { width: 1360, height: 900 },
  },
});

import { defineConfig, devices } from "@playwright/test";

const port = Number(process.env.FIXTURE_PORT ?? 4391);

// Browser tests drive the built UI (dist/) against the test-only fixture
// server. They run serially because the fixture holds one shared state.
export default defineConfig({
  testDir: "test/browser",
  fullyParallel: false,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "desktop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1360, height: 900 } },
    },
  ],
  webServer: {
    command: `node test/fixture-server/main.ts ${port}`,
    url: `http://127.0.0.1:${port}/__fixture/health`,
    reuseExistingServer: false,
  },
});

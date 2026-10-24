import { expect, test, type Page } from "@playwright/test";
import { control, expectNoHorizontalScroll, reset, shot } from "./support.ts";

const entry = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "Workspaces" })
    .getByRole("listitem")
    .filter({
      has: page.getByRole("heading", { name, exact: true }),
    });

const jobIdFromUrl = (page: Page) => new URL(page.url()).searchParams.get("job") ?? "";

test.beforeEach(async ({ request }) => reset(request));

test("lists every workspace folder with its container state", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  const rows = page.getByRole("list", { name: "Workspaces" }).getByRole("listitem");
  await expect(rows).toHaveCount(5);
  await expect(entry(page, "sample-alpha").locator(".stamp")).toHaveText("Running");
  await expect(entry(page, "sample-beta").locator(".stamp")).toHaveText("Stopped");
  await expect(entry(page, "sample-gamma").locator(".stamp")).toHaveText("No container");
  await expect(entry(page, "sample-gamma")).toContainText("no container");
  await expect(entry(page, "sample-alpha")).toContainText(/container [0-9a-f]{12}/);
  // Actions follow the container state.
  await expect(entry(page, "sample-alpha").getByRole("button", { name: "Start" })).toBeDisabled();
  await expect(entry(page, "sample-alpha").getByRole("button", { name: "Stop" })).toBeEnabled();
  await expect(entry(page, "sample-beta").getByRole("button", { name: "Stop" })).toBeDisabled();
  await expect(entry(page, "sample-gamma").getByRole("button", { name: "Start" })).toBeEnabled();
  await expect(entry(page, "sample-gamma").getByRole("link", { name: "Upgrade" })).toHaveAttribute(
    "href",
    "/workspaces/sample-gamma/upgrade",
  );
  await page.waitForTimeout(700); // let the entry animation settle for the screenshot
  await shot(page, "list-desktop");
});

test("starts a stopped workspace and streams its job to completion", async ({ page }) => {
  await page.goto("/");
  await entry(page, "sample-beta").getByRole("button", { name: "Start" }).click();
  const console = page.getByRole("region", { name: "Job console" });
  await expect(console.getByRole("heading", { name: /Start sample-beta/ })).toBeVisible();
  await expect(console.locator(".outcome")).toContainText("Completed.");
  await expect(console.getByRole("list", { name: "Phases" })).toContainText("initialize-command");
  await expect(console.getByRole("list", { name: "Phases" })).toHaveText(
    /read-definition.*initialize-command.*up/s,
  );
  await expect(console.getByRole("log", { name: "Job log" })).toContainText("initialize: ok");
  await expect(entry(page, "sample-beta").locator(".stamp")).toHaveText("Running");
  await expect(page.getByRole("region", { name: "Jobs since Menagerie started" })).toContainText(
    "sample-beta start",
  );
  await shot(page, "list-job-succeeded");
});

test("allows one job per workspace while other workspaces stay usable", async ({
  page,
  request,
}) => {
  await page.goto("/");
  await entry(page, "sample-slow").getByRole("button", { name: "Rebuild" }).click();
  // Rebuild asks for confirmation first.
  await entry(page, "sample-slow").getByRole("button", { name: "Confirm rebuild" }).click();
  await expect(page.getByRole("list", { name: "Phases" })).toContainText("build");
  const slowJob = jobIdFromUrl(page);
  expect(slowJob).not.toBe("");

  const slow = entry(page, "sample-slow");
  await expect(slow.getByRole("link", { name: /rebuilding · running/ })).toBeVisible();
  for (const name of ["Start", "Stop", "Rebuild"]) {
    await expect(slow.getByRole("button", { name })).toBeDisabled();
  }
  await expect(slow.getByText("Upgrade")).toHaveAttribute("aria-disabled", "true");

  // The service refuses a second job for the same workspace.
  const refused = await request.post("/api/jobs", {
    data: { operation: "stop", workspace: "sample-slow" },
  });
  expect(refused.status()).toBe(409);
  expect((await refused.json()).error).toMatchObject({
    kind: "workspace-busy",
    activeJobId: slowJob,
  });

  // Another workspace runs its own job meanwhile.
  await entry(page, "sample-alpha").getByRole("button", { name: "Stop" }).click();
  await expect(page.getByRole("region", { name: "Job console" }).locator(".outcome")).toContainText(
    "Completed.",
  );
  await expect(entry(page, "sample-alpha").locator(".stamp")).toHaveText("Stopped");
  await expect(slow.getByRole("button", { name: "Stop" })).toBeDisabled();
  await shot(page, "list-busy-workspace");

  await control(request, `release/${slowJob}`);
  await page.getByRole("link", { name: /rebuilding/ }).click();
  await expect(page.getByRole("region", { name: "Job console" }).locator(".outcome")).toContainText(
    "Completed.",
  );
  await expect(slow.getByRole("button", { name: "Stop" })).toBeEnabled();
});

test("reports a busy workspace when the list was stale", async ({ page, request }) => {
  await page.goto("/");
  await expect(entry(page, "sample-slow").getByRole("button", { name: "Stop" })).toBeEnabled();
  const started = await request.post("/api/jobs", {
    data: { operation: "stop", workspace: "sample-slow" },
  });
  const { jobId } = await started.json();
  await entry(page, "sample-slow").getByRole("button", { name: "Stop" }).click();
  const error = entry(page, "sample-slow").getByRole("alert");
  await expect(error).toContainText("already has a stop job");
  await error.getByRole("link", { name: "Show the running job" }).click();
  await expect(page).toHaveURL(new RegExp(`job=${jobId}`));
  await control(request, `release/${jobId}`);
});

test("cancels a rebuild at the confirmation", async ({ page, request }) => {
  await page.goto("/");
  await entry(page, "sample-alpha").getByRole("button", { name: "Rebuild" }).click();
  await expect(
    entry(page, "sample-alpha").getByRole("button", { name: "Confirm rebuild" }),
  ).toBeFocused();
  await entry(page, "sample-alpha").getByRole("button", { name: "Keep" }).click();
  await expect(entry(page, "sample-alpha").getByRole("button", { name: "Rebuild" })).toBeVisible();
  const jobs = await (await request.get("/api/jobs")).json();
  expect(jobs.jobs).toHaveLength(0);
});

test("shows a failed job with its error and log", async ({ page }) => {
  await page.goto("/");
  await entry(page, "sample-broken").getByRole("button", { name: "Start" }).click();
  const console = page.getByRole("region", { name: "Job console" });
  await expect(console.locator(".console-head .stamp")).toHaveText("Failed");
  await expect(console.locator(".outcome")).toContainText("Failed.");
  await expect(console.locator(".outcome")).toContainText("initialize-command-failed");
  await expect(console.locator(".outcome")).toContainText("initializeCommand exited with status 3");
  await expect(console.getByRole("log")).toContainText("initialize: missing sample marker");
  await expect(entry(page, "sample-broken").locator(".stamp")).toHaveText("Stopped");
  await shot(page, "list-job-failed");
});

test("replays a job's history on reload without doubling lines", async ({ page, request }) => {
  await page.goto("/");
  await entry(page, "sample-slow").getByRole("button", { name: "Rebuild" }).click();
  await entry(page, "sample-slow").getByRole("button", { name: "Confirm rebuild" }).click();
  const log = page.getByRole("log", { name: "Job log" });
  await expect(log).toContainText("#30 building layer 30 of 30");
  const before = await log.getByRole("listitem").count();
  await page.reload();
  await expect(log).toContainText("#30 building layer 30 of 30");
  expect(await log.getByRole("listitem").count()).toBe(before);
  // Live events still arrive after the replay.
  await control(request, `release/${jobIdFromUrl(page)}`);
  await expect(page.getByRole("region", { name: "Job console" }).locator(".outcome")).toContainText(
    "Completed.",
  );
  expect(await log.getByRole("listitem").count()).toBeGreaterThan(before);
});

test("explains a job that a Menagerie restart dropped", async ({ page, request }) => {
  await page.goto("/");
  await entry(page, "sample-beta").getByRole("button", { name: "Start" }).click();
  await expect(page.getByRole("region", { name: "Job console" }).locator(".outcome")).toContainText(
    "Completed.",
  );
  const jobId = jobIdFromUrl(page);
  await control(request, "restart");
  await page.goto(`/?job=${jobId}`);
  await expect(page.getByRole("status")).toContainText("Menagerie no longer knows this job");
  await expect(page.getByRole("list", { name: "Workspaces" }).getByRole("listitem")).toHaveCount(5);
});

test("shows an unreachable Docker daemon", async ({ page, request }) => {
  await control(request, "docker", { available: false });
  await page.goto("/");
  await expect(page.getByRole("alert")).toContainText("docker-unavailable");
  await expect(page.getByRole("alert")).toContainText("The Docker daemon is not reachable.");
});

test("sends a signed-out person to the login and back", async ({ page, request }) => {
  await control(request, "auth", { signedIn: false });
  await page.goto("/workspaces/sample-alpha/upgrade");
  await expect(page).toHaveURL(/\/auth\/login\?return=%2Fworkspaces%2Fsample-alpha%2Fupgrade$/);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/workspaces\/sample-alpha\/upgrade$/);
  await expect(page.getByRole("heading", { name: "Upgrade sample-alpha" })).toBeVisible();
});

test("signs out with a POST to the logout route", async ({ page }) => {
  await page.goto("/");
  const request = page.waitForRequest((r) => r.url().endsWith("/auth/logout"));
  await page.getByRole("button", { name: "Sign out" }).click();
  expect((await request).method()).toBe("POST");
  await expect(page).toHaveURL(/\/auth\/login/);
});

test.describe("narrow viewport", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("stacks the register without sideways scrolling", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByRole("list", { name: "Workspaces" }).getByRole("listitem")).toHaveCount(
      5,
    );
    await expectNoHorizontalScroll(page);
    const actions = entry(page, "sample-alpha").getByRole("group", {
      name: "Actions for sample-alpha",
    });
    const nameBox = await entry(page, "sample-alpha").getByRole("heading").boundingBox();
    const actionsBox = await actions.boundingBox();
    expect(actionsBox && nameBox && actionsBox.y > nameBox.y + nameBox.height).toBeTruthy();
    await page.waitForTimeout(700);
    await shot(page, "list-narrow");
    await entry(page, "sample-broken").getByRole("button", { name: "Start" }).click();
    await expect(
      page.getByRole("region", { name: "Job console" }).locator(".outcome"),
    ).toContainText("Failed.");
    await expectNoHorizontalScroll(page);
    await shot(page, "list-narrow-job");
  });
});

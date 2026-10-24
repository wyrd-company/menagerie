import { expect, test, type Page } from "@playwright/test";
import rawUpgrade from "../fixtures/toha/documents/upgrade-batch-1.json" with { type: "json" };
import { control, expectNoHorizontalScroll, reset, shot } from "./support.ts";

test.beforeEach(async ({ request }) => reset(request));

async function beginUpgrade(page: Page, workspace: string) {
  await page.goto(`/workspaces/${workspace}/upgrade`);
  await expect(page.getByRole("heading", { name: `Upgrade ${workspace}` })).toBeVisible();
  await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await expect(page.getByText("Question batch 1")).toBeVisible();
}

test("renders the whole producer batch it receives and completes the upgrade job", async ({
  page,
  request,
}) => {
  await page.goto("/workspaces/sample-alpha/upgrade");
  await expect(page.locator(".upgrade-current .stamp")).toHaveText("Running");
  await shot(page, "upgrade-start");
  await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await expect(page.getByText("Question batch 1")).toBeVisible();
  await expect(page).toHaveURL(/\/workspaces\/sample-alpha\/upgrade\?job=job-/);

  // This is the raw `stage --update` batch: every property is a question, none filtered.
  const ids = Object.keys(rawUpgrade.schema.properties);
  expect(ids).toHaveLength(11);
  for (const id of ids) await expect(page.locator(`[data-question="${id}"]`)).toHaveCount(1);
  await expect(page.locator("[data-question]")).toHaveCount(ids.length);
  await expect(page.getByLabel(/^Title/)).toHaveValue("Sample project");
  await expect(page.getByRole("textbox", { name: "Label 2" })).toHaveValue("two");
  await expect(page.getByRole("group", { name: /^Region/ }).getByRole("radio")).toHaveCount(2);
  await expect(
    page.getByRole("region", { name: "Upgrade job" }).getByRole("list", { name: "Phases" }),
  ).toContainText("backup");
  await shot(page, "upgrade-raw-batch");

  await page
    .getByRole("group", { name: /^Region/ })
    .getByRole("radio", { name: "north" })
    .check();
  await page
    .getByLabel(/^Settings document/)
    .fill('{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample"}');
  await page.getByRole("button", { name: "Send answers" }).click();
  const job = page.getByRole("region", { name: "Upgrade job" });
  await expect(job.locator(".outcome")).toContainText("Completed.");
  await expect(job.getByRole("list", { name: "Phases" })).toHaveText(
    /backup.*interview.*apply.*initialize-command.*rebuild/s,
  );
  await expect(job.getByRole("table", { name: "Files" })).toContainText("update");
  const sent = (await (await request.get("/__fixture/requests")).json()).answers[0];
  expect(JSON.parse(sent.last).answers).toEqual({
    region: "north",
    settings: { $schema: "https://schemas.example.test/sample-settings/1", name: "sample" },
  });
  await shot(page, "upgrade-completed");
});

test("asks only what a narrowed batch asks (synthetic uncovered-question fixture)", async ({
  page,
  request,
}) => {
  // upgrade-batch-uncovered.json is the producer document narrowed by the generator;
  // it shows rendering of a one-question batch, not Toha's replay behaviour.
  await control(request, "options", { upgradeDocument: "uncovered" });
  await beginUpgrade(page, "sample-gamma");
  await expect(page.locator("[data-question]")).toHaveCount(1);
  await expect(page.getByRole("group", { name: /^Region/ })).toContainText(
    "Added by version 2 of the sample template.",
  );
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(page.locator(".error-summary")).toContainText("Region: is required");
  await page.getByRole("radio", { name: "south" }).check();
  await page.getByRole("radio", { name: "south" }).press("Control+Enter");
  await expect(page.getByRole("region", { name: "Upgrade job" }).locator(".outcome")).toContainText(
    "Completed.",
  );
});

test("reports merge conflicts as a failed upgrade with the conflicted paths", async ({ page }) => {
  await page.goto("/workspaces/sample-beta/upgrade");
  await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await page
    .getByRole("group", { name: /^Region/ })
    .getByRole("radio", { name: "north" })
    .check();
  await page
    .getByLabel(/^Settings document/)
    .fill('{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample"}');
  await page.getByRole("button", { name: "Send answers" }).click();
  const job = page.getByRole("region", { name: "Upgrade job" });
  await expect(job.locator(".outcome")).toContainText("Failed.");
  await expect(job.locator(".outcome")).toContainText("merge-conflicted");
  await expect(job.locator(".apply")).toContainText("conflicted");
  await expect(job.locator(".apply .notice")).toContainText("README.md");
  await shot(page, "upgrade-conflicted");
});

test("keeps an upgrade off a workspace that already has a job", async ({ page, request }) => {
  const started = await request.post("/api/jobs", {
    data: { operation: "stop", workspace: "sample-slow" },
  });
  const { jobId } = await started.json();
  await page.goto("/workspaces/sample-slow/upgrade");
  await expect(page.getByRole("status")).toContainText("already has a stop job");
  await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
  await expect(page.getByRole("button", { name: "Begin upgrade" })).toBeDisabled();
  await page.getByRole("link", { name: "Show the running job" }).click();
  await expect(page).toHaveURL(new RegExp(`/\\?job=${jobId}$`));
  await control(request, `release/${jobId}`);
});

test("names a workspace folder that does not exist", async ({ page }) => {
  await page.goto("/workspaces/sample-missing/upgrade");
  await expect(page.getByRole("alert")).toContainText("No folder named sample-missing");
});

test.describe("narrow viewport", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("fits the upgrade interview", async ({ page }) => {
    await beginUpgrade(page, "sample-alpha");
    await expectNoHorizontalScroll(page);
    await shot(page, "upgrade-narrow");
  });
});

import { expect, test, type Page } from "@playwright/test";
import { control, expectNoHorizontalScroll, reset, shot } from "./support.ts";

const entry = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "Workspaces" })
    .getByRole("listitem")
    .filter({
      has: page.getByRole("heading", { name, exact: true }),
    });

async function openApply(page: Page, name: string) {
  await entry(page, name).getByRole("button", { name: "Apply branch" }).click();
  const form = entry(page, name).getByRole("form", { name: `Apply a branch to ${name}` });
  await expect(form.getByRole("textbox", { name: "Branch" })).toBeFocused();
  return form;
}

test.beforeEach(async ({ request }) => reset(request));

test("applies a named branch and streams the job through the rebuild", async ({ page }) => {
  const posted: unknown[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith("/api/jobs") && r.method() === "POST") posted.push(r.postDataJSON());
  });
  await page.goto("/");
  const form = await openApply(page, "sample-alpha");
  const apply = form.getByRole("button", { name: "Apply" });
  await expect(apply).toBeDisabled();
  await form.getByRole("textbox", { name: "Branch" }).fill(" review/devcontainer ");
  await expect(apply).toBeEnabled();
  await shot(page, "apply-branch-form");
  await apply.click();

  const console = page.getByRole("region", { name: "Job console" });
  await expect(
    console.getByRole("heading", { name: /Apply branch to sample-alpha/ }),
  ).toBeVisible();
  await expect(console.locator(".outcome")).toContainText("Completed.");
  await expect(entry(page, "sample-alpha").getByRole("form")).toHaveCount(0);
  await expect(console.getByRole("list", { name: "Phases" })).toHaveText(
    /fetch.*backup-local-changes.*backup.*apply.*rebuild/s,
  );
  await expect(console.getByRole("log", { name: "Job log" })).toContainText(
    "fetched branch review/devcontainer",
  );
  expect(posted).toEqual([
    { operation: "apply-branch", workspace: "sample-alpha", branch: "review/devcontainer" },
  ]);
  await expect(page.getByRole("region", { name: "Jobs since Menagerie started" })).toContainText(
    "sample-alpha apply-branch",
  );
  await shot(page, "apply-branch-succeeded");
});

test("shows a refused apply with the paths in the way", async ({ page }) => {
  await page.goto("/");
  const form = await openApply(page, "sample-beta");
  await form.getByRole("textbox", { name: "Branch" }).fill("conflicting");
  await form.getByRole("textbox", { name: "Branch" }).press("Enter");
  const console = page.getByRole("region", { name: "Job console" });
  await expect(console.locator(".outcome")).toContainText("Failed.");
  await expect(console.locator(".outcome")).toContainText("not-applicable");
  await expect(console.locator(".outcome")).toContainText("[paths: README.md]");
  await expect(console.getByRole("log")).toContainText("not applicable: README.md");
  await shot(page, "apply-branch-not-applicable");
});

test("shows a branch the workspace repository does not have", async ({ page }) => {
  await page.goto("/");
  const form = await openApply(page, "sample-beta");
  await form.getByRole("textbox", { name: "Branch" }).fill("missing");
  await form.getByRole("button", { name: "Apply" }).click();
  const console = page.getByRole("region", { name: "Job console" });
  await expect(console.locator(".outcome")).toContainText("branch-not-found");
});

test("cancels the apply without starting a job", async ({ page, request }) => {
  await page.goto("/");
  const form = await openApply(page, "sample-alpha");
  await form.getByRole("textbox", { name: "Branch" }).fill("review/devcontainer");
  await form.getByRole("button", { name: "Cancel" }).click();
  await expect(
    entry(page, "sample-alpha").getByRole("button", { name: "Apply branch" }),
  ).toBeVisible();
  // A whitespace-only name starts nothing either.
  const again = await openApply(page, "sample-alpha");
  await expect(again.getByRole("textbox", { name: "Branch" })).toHaveValue("");
  await again.getByRole("textbox", { name: "Branch" }).fill("   ");
  await expect(again.getByRole("button", { name: "Apply" })).toBeDisabled();
  await again.getByRole("textbox", { name: "Branch" }).press("Enter");
  const jobs = await (await request.get("/api/jobs")).json();
  expect(jobs.jobs).toHaveLength(0);
});

test("keeps one inline confirmation open per row", async ({ page }) => {
  await page.goto("/");
  await openApply(page, "sample-alpha");
  await entry(page, "sample-alpha").getByRole("button", { name: "Rebuild" }).click();
  await expect(entry(page, "sample-alpha").getByRole("form")).toHaveCount(0);
  await entry(page, "sample-alpha").getByRole("button", { name: "Apply branch" }).click();
  await expect(
    entry(page, "sample-alpha").getByRole("button", { name: "Confirm rebuild" }),
  ).toHaveCount(0);
});

test("disables an open apply once the workspace has a job", async ({ page, request }) => {
  await page.goto("/");
  const form = await openApply(page, "sample-slow");
  await form.getByRole("textbox", { name: "Branch" }).fill("review/devcontainer");
  await expect(form.getByRole("button", { name: "Apply" })).toBeEnabled();
  const started = await request.post("/api/jobs", {
    data: { operation: "stop", workspace: "sample-slow" },
  });
  const { jobId } = await started.json();
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(form.getByRole("button", { name: "Apply" })).toBeDisabled();
  await control(request, `release/${jobId}`);
});

test("offers no apply while the workspace has a job", async ({ page }) => {
  await page.goto("/");
  await entry(page, "sample-slow").getByRole("button", { name: "Rebuild" }).click();
  await entry(page, "sample-slow").getByRole("button", { name: "Confirm rebuild" }).click();
  await expect(entry(page, "sample-slow").getByRole("link", { name: /rebuilding/ })).toBeVisible();
  await expect(
    entry(page, "sample-slow").getByRole("button", { name: "Apply branch" }),
  ).toBeDisabled();
});

test.describe("narrow viewport", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("fits the branch field without sideways scrolling", async ({ page }) => {
    await page.goto("/");
    const form = await openApply(page, "sample-alpha");
    await form
      .getByRole("textbox", { name: "Branch" })
      .fill("review/a-long-branch-name/devcontainer");
    await expectNoHorizontalScroll(page);
    await shot(page, "apply-branch-narrow");
  });
});

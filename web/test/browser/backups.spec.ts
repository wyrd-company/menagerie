import { expect, test, type Page } from "@playwright/test";
import { expectNoHorizontalScroll, reset, shot } from "./support.ts";

test.beforeEach(async ({ request }) => reset(request));

test("registers a backup repository and creates a workspace that backs up to it", async ({
  page,
}) => {
  await page.goto("/");
  const alpha = page.getByRole("listitem").filter({ hasText: "sample-alpha" });
  await expect(alpha.getByText("Backs up to Example backups")).toBeVisible();
  await expect(alpha.getByText("Backs up to Example backups")).toHaveAttribute(
    "title",
    "ssh://git@git.example.test/example-org/workspace-backups.git",
  );

  await page.getByRole("link", { name: "Backups" }).click();
  await expect(page.getByRole("heading", { name: "Backup repositories", level: 1 })).toBeVisible();
  const example = page.getByRole("listitem").filter({ hasText: "Example backups" });
  await expect(example.getByText(/Backs up sample-alpha/)).toBeVisible();
  await expect(example.getByRole("button", { name: "Remove" })).toBeDisabled();
  await shot(page, "backups-list");

  await page.getByRole("button", { name: "Register repository" }).first().click();
  const save = page.getByRole("button", { name: "Save" });
  await page.getByLabel("Display name").fill("Client backups");
  await page.getByLabel("Git URL").fill("git@git.example.test:client-org/readonly.git");
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.locator("#backup-registration-error")).toHaveText(
    "write: Menagerie could not write to the repository: the workspace repository denied access",
  );
  await expect(save).toBeDisabled();
  await shot(page, "backups-register-refused");
  await page.getByLabel("Git URL").fill("git@git.example.test:client-org/backups.git");
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.getByRole("status")).toHaveText(
    "Checks passed · Menagerie can write to it, as client-backups",
  );
  await shot(page, "backups-register-validated");
  await save.click();
  const client = page.getByRole("listitem").filter({ hasText: "Client backups" });
  await expect(client.getByText("No workspace backs up to it")).toBeVisible();

  await page.getByRole("link", { name: "New workspace" }).click();
  await page.getByRole("radio", { name: /Sample kit/ }).check();
  await page.getByLabel("Workspace name").fill("client-space");
  // Two are registered, so the person chooses one.
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.getByText("Choose a backup repository.")).toBeVisible();
  await page.getByRole("radio", { name: /Client backups/ }).check();
  await shot(page, "backups-create-choice");
  const posted = page.waitForRequest((r) => r.url().endsWith("/api/jobs") && r.method() === "POST");
  await page.getByRole("button", { name: "Begin interview" }).click();
  expect((await posted).postDataJSON()).toMatchObject({
    operation: "create",
    workspace: "client-space",
    backupRepositoryId: "client-backups",
  });
  await expect(page.getByText("Question batch 1")).toBeVisible();
});

test.describe("narrow viewport", () => {
  test.use({ viewport: { width: 390, height: 844 } });
  test("fits the backup repositories screen", async ({ page }) => {
    await page.goto("/backup-repositories");
    await expect(page.getByRole("heading", { name: "Example backups" })).toBeVisible();
    await expectNoHorizontalScroll(page);
  });
});

const entry = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "Workspaces" })
    .getByRole("listitem")
    .filter({ has: page.getByRole("heading", { name, exact: true }) });

test("moves a workspace's backups, and keeps its repository when the move fails", async ({
  page,
}) => {
  for (const [name, url] of [
    ["Client backups", "git@git.example.test:client-org/backups.git"],
    ["Taken backups", "git@git.example.test:client-org/conflict.git"],
  ])
    expect(
      (await page.request.post("/api/backup-repositories", { data: { name, url } })).ok(),
    ).toBe(true);
  await page.goto("/");
  const row = entry(page, "sample-beta");
  await row.getByRole("button", { name: "Move backups" }).click();
  const form = row.getByRole("form", { name: "Move the backups of sample-beta" });
  const select = form.getByRole("combobox", { name: "Move backups to" });
  await expect(select.getByRole("option", { name: /Example backups/ })).toHaveCount(0);
  await select.selectOption("taken-backups");
  await shot(page, "move-backup-form");
  await form.getByRole("button", { name: "Move backups" }).click();
  const console = page.getByRole("region", { name: "Job console" });
  await expect(
    console.getByRole("heading", { name: /Move the backups of sample-beta/ }),
  ).toBeVisible();
  await expect(console).toContainText("backup-branch-conflict");
  await expect(console).toContainText("the workspace still backs up to Example backups");
  await shot(page, "move-backup-conflict");
  await expect(row.getByText("Backs up to Example backups")).toBeVisible();

  await row.getByRole("button", { name: "Move backups" }).click();
  await form.getByRole("combobox", { name: "Move backups to" }).selectOption("client-backups");
  await form.getByRole("button", { name: "Move backups" }).click();
  await expect(console.locator(".outcome")).toContainText("Completed.");
  await expect(console.getByRole("list", { name: "Phases" })).toHaveText(/verify.*commit.*backup/s);
  await expect(row.getByText("Backs up to Client backups")).toBeVisible();
  await shot(page, "move-backup-succeeded");
});

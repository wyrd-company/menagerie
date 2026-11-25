import { expect, test } from "@playwright/test";
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

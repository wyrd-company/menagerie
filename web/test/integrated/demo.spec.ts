// The README's demo and the documentation's screenshots, recorded against
// a fresh acceptance fixture by docs/demo/record.sh. The fixture's host is
// prepared for provisioning. A manager creates a workspace whose template
// declares its credentials, pairs T3 Code, upgrades it and deletes it.
import { expect, test, type Page } from "@playwright/test";
import { console_, entry, finished, signIn } from "./support.ts";

const assets = process.env.MENAGERIE_DEMO_ASSETS!;
const pause = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
const shot = (page: Page, name: string) =>
  page.screenshot({ path: `${assets}/${name}.png`, animations: "disabled" });
// Test-only bound on a create that builds the T3 Code Feature's image.
const firstBuild = 30 * 60_000;

async function beginCreate(page: Page, template: RegExp, name: string, delay = 0) {
  await page.getByRole("link", { name: "New workspace" }).first().click();
  await page.getByRole("radio", { name: template }).check();
  await page.getByLabel("Workspace name").pressSequentially(name, { delay });
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.locator("[data-question]")).toHaveCount(4);
  await page.getByLabel(/^Label/).pressSequentially("Sample project", { delay });
}

// The first workspace builds the image, so the recording's create reads the
// cache, and the list has more than the recording's own row. This page records
// no video.
test.beforeAll(async ({ browser }) => {
  test.setTimeout(firstBuild + 5 * 60_000);
  const page = await browser.newPage();
  await signIn(page, "sample-manager");
  await beginCreate(page, /Workspace kit, version 1/, "sample-tools");
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.", {
    timeout: firstBuild,
  });
  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  await beginCreate(page, /Sample kit, version 1/, "sample-notes");
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  await entry(page, "sample-notes").getByRole("button", { name: "Stop" }).click();
  await finished(page, /sample-notes/);
  await page.close();
});

test("create, pair, upgrade and delete a provisioned workspace", async ({ page }) => {
  await signIn(page, "sample-manager");
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  await pause(1500);

  await beginCreate(page, /Workspace kit, version 1/, "sample-space", 60);
  await page.getByRole("group", { name: /^Size/ }).getByRole("radio", { name: "large" }).check();
  await shot(page, "create-interview");
  await pause(1000);
  await page.getByLabel(/^Label/).press("Control+Enter");
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  for (const phase of ["issue-key", "bind", "create-container", "wait-for-sockets", "verify"]) {
    await expect(console_(page)).toContainText(phase);
  }
  await expect(console_(page)).toContainText("check-credentials: the Xolotl key reads");
  await shot(page, "provisioning-console");
  await pause(3000);

  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  const row = entry(page, "sample-space");
  await expect(row.locator(".status")).toHaveText("Running");
  await pause(1500);

  // No image shows a pairing token: a blank block stands in for the text.
  await page.addStyleTag({
    content: ".entry-token code { color: transparent !important; background: #cdd5df; }",
  });
  await row.getByRole("button", { name: "T3 Code token" }).click();
  const token = row.getByRole("group", { name: "T3 Code pairing token" });
  await expect(token.locator("code")).toHaveCSS("color", "rgba(0, 0, 0, 0)");
  await expect(token.locator("code")).not.toBeEmpty();
  await shot(page, "t3-code-token");
  await pause(2000);
  await token.getByRole("button", { name: "Dismiss" }).click();
  await pause(1000);

  await row.getByRole("link", { name: "Upgrade" }).click();
  await page.getByRole("radio", { name: /Workspace kit, version 2/ }).check();
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await expect(page.locator("[data-question]")).toHaveCount(1);
  await page.getByRole("radio", { name: "north" }).check();
  await shot(page, "upgrade-interview");
  await pause(1000);
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  await pause(2000);

  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  await expect(row.locator(".status")).toHaveText("Running");
  await shot(page, "list");
  await pause(1500);

  await row.getByRole("button", { name: "Delete" }).click();
  const remove = page.getByRole("form", { name: "Delete sample-space" });
  await remove.getByRole("textbox").pressSequentially("sample-space", { delay: 60 });
  await shot(page, "delete-workspace");
  await pause(1000);
  await remove.getByRole("button", { name: "Delete workspace" }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  for (const phase of ["unbind", "remove-container", "revoke-key", "remove-folder"]) {
    await expect(console_(page)).toContainText(phase);
  }
  await expect(row).toHaveCount(0);
  await pause(3000);
  await pause(1500);
});

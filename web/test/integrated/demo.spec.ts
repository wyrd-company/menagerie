// The README's demo and the documentation's screenshots, recorded against
// a fresh acceptance fixture by docs/demo/record.sh: a manager creates a
// workspace through its interview, then upgrades it with one new question.
import { expect, test } from "@playwright/test";
import { console_, entry, signIn } from "./support.ts";

const assets = process.env.MENAGERIE_DEMO_ASSETS!;
const pause = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

test("create and upgrade a workspace", async ({ page }) => {
  await signIn(page, "sample-manager");
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  await pause(1500);

  await page.getByRole("link", { name: "New workspace" }).first().click();
  await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
  await page.getByLabel("Workspace name").pressSequentially("sample-space", { delay: 60 });
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.locator("[data-question]")).toHaveCount(4);
  await page.getByLabel(/^Label/).pressSequentially("Sample project", { delay: 60 });
  await page.getByRole("group", { name: /^Size/ }).getByRole("radio", { name: "large" }).check();
  await page.screenshot({ path: `${assets}/create-interview.png`, animations: "disabled" });
  await pause(1000);
  await page.getByLabel(/^Label/).press("Control+Enter");
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  await pause(2000);

  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  await expect(entry(page, "sample-space").locator(".stamp")).toHaveText("Running");
  await pause(1500);
  await entry(page, "sample-space").getByRole("link", { name: "Upgrade" }).click();
  await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await expect(page.locator("[data-question]")).toHaveCount(1);
  await page.getByRole("radio", { name: "north" }).check();
  await page.screenshot({ path: `${assets}/upgrade-interview.png`, animations: "disabled" });
  await pause(1000);
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  await pause(2000);

  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  await expect(entry(page, "sample-space").locator(".stamp")).toHaveText("Running");
  await page.screenshot({ path: `${assets}/list.png`, animations: "disabled" });
  await pause(1500);
});

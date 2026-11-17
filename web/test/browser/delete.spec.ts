import { expect, test, type Page } from "@playwright/test";
import { expectNoHorizontalScroll, reset, shot } from "./support.ts";

const entry = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "Workspaces" })
    .getByRole("listitem")
    .filter({
      has: page.getByRole("heading", { name, exact: true }),
    });

async function openDelete(page: Page, name: string) {
  await entry(page, name).getByRole("button", { name: "Delete", exact: true }).click();
  const form = entry(page, name).getByRole("form", { name: `Delete ${name}` });
  await expect(form.getByRole("textbox")).toBeFocused();
  return form;
}

test.beforeEach(async ({ request }) => reset(request));

test("deletes a workspace only after its name is typed", async ({ page }) => {
  const posted: unknown[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith("/api/jobs") && r.method() === "POST") posted.push(r.postDataJSON());
  });
  await page.goto("/");
  const form = await openDelete(page, "sample-alpha");
  await expect(form).toContainText("Ignored files");
  await expect(form).toContainText("are not backed up");
  const confirm = form.getByRole("button", { name: "Delete workspace" });

  await expect(confirm).toBeDisabled();
  await form.getByRole("textbox").fill("sample-alph");
  await expect(confirm).toBeDisabled();
  await form.getByRole("textbox").press("Enter");
  expect(posted).toEqual([]);
  await form.getByRole("textbox").fill("sample-alpha");
  await expect(confirm).toBeEnabled();
  await expectNoHorizontalScroll(page);
  await shot(page, "delete-workspace-form");
  await confirm.click();

  const console = page.getByRole("region", { name: "Job console" });
  await expect(console.getByRole("heading", { name: /Delete sample-alpha/ })).toBeVisible();
  await expect(console.locator(".outcome")).toContainText("Completed.");
  expect(posted).toEqual([{ operation: "delete", workspace: "sample-alpha" }]);
  await expect(entry(page, "sample-alpha")).toHaveCount(0);
});

test("offers only Delete for a workspace whose folder is gone, and deletes it", async ({
  page,
}) => {
  const posted: unknown[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith("/api/jobs") && r.method() === "POST") posted.push(r.postDataJSON());
  });
  await page.goto("/");
  const gone = entry(page, "sample-gone");
  await expect(gone.locator(".status")).toHaveText("No folder");
  await expect(gone).toContainText("folder gone");
  for (const action of ["Start", "Stop", "Rebuild", "Apply branch", "T3 Code token"]) {
    await expect(gone.getByRole("button", { name: action, exact: true })).toBeDisabled();
  }
  await expect(gone.getByRole("link", { name: "Upgrade" })).toHaveCount(0);
  const form = await openDelete(page, "sample-gone");
  await expect(form).toContainText("is gone");
  await expect(form).toContainText("binding and the Xolotl key");
  await form.getByRole("textbox").fill("sample-gone");
  await shot(page, "delete-no-folder-form");
  await form.getByRole("button", { name: "Delete workspace" }).click();

  const console = page.getByRole("region", { name: "Job console" });
  await expect(console.locator(".outcome")).toContainText("Completed.");
  await expect(console).toContainText("binding of devcontainer-sample-gone is gone");
  expect(posted).toEqual([{ operation: "delete", workspace: "sample-gone" }]);
  await expect(entry(page, "sample-gone")).toHaveCount(0);
});

test("keeps the workspace and clears the typed name", async ({ page }) => {
  await page.goto("/");
  const form = await openDelete(page, "sample-alpha");
  await form.getByRole("textbox").fill("sample-alpha");
  await form.getByRole("button", { name: "Keep" }).click();
  await expect(entry(page, "sample-alpha").getByRole("form")).toHaveCount(0);
  const again = await openDelete(page, "sample-alpha");
  await expect(again.getByRole("textbox")).toHaveValue("");
});

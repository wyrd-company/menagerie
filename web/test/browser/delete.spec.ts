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

test("keeps the workspace and clears the typed name", async ({ page }) => {
  await page.goto("/");
  const form = await openDelete(page, "sample-alpha");
  await form.getByRole("textbox").fill("sample-alpha");
  await form.getByRole("button", { name: "Keep" }).click();
  await expect(entry(page, "sample-alpha").getByRole("form")).toHaveCount(0);
  const again = await openDelete(page, "sample-alpha");
  await expect(again.getByRole("textbox")).toHaveValue("");
});

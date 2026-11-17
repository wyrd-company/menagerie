import { expect, test } from "@playwright/test";
import { reset, shot, expectNoHorizontalScroll } from "./support.ts";

test.beforeEach(async ({ request }) => {
  await reset(request);
});
test("required policies and group settings reach every workspace selection", async ({ page }) => {
  await page.goto("/policies");
  await page.getByRole("checkbox", { name: "workspace required", exact: true }).check();
  await page.getByRole("button", { name: "Add group" }).click();
  const group = page.getByRole("region", { name: "Group 1" });
  await group.getByRole("textbox", { name: "Group name" }).fill("Operator SSH");
  await group.getByRole("combobox", { name: "Selection" }).selectOption("multi");
  await group.getByRole("checkbox", { name: "bob-ssh", exact: true }).check();
  await group.getByRole("checkbox", { name: "ira-ssh", exact: true }).check();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("status")).toHaveText("Saved");
  await page.getByRole("link", { name: "Workspaces", exact: true }).click();
  await expect(page.getByLabel("Policies for sample-alpha", { exact: true })).toHaveText(
    "workspace",
  );
  await expect(page.getByText("Needs choice: Operator SSH")).toHaveCount(6);
  await page.getByRole("link", { name: "Policies", exact: true }).click();
  await expect(
    page.getByRole("checkbox", { name: "workspace required", exact: true }),
  ).toBeChecked();
  await page.getByRole("button", { name: "Remove group" }).click();
  await page.getByRole("checkbox", { name: "workspace required", exact: true }).uncheck();
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByRole("status")).toHaveText("Saved");
  await page.getByRole("link", { name: "Workspaces", exact: true }).click();
  await expect(page.getByLabel("Policies for sample-alpha", { exact: true })).toHaveText(
    "workspace",
  );
  await expect(page.getByText("Needs choice: Operator SSH")).toHaveCount(0);
});
test("an empty selection is visible", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByLabel("Policies for sample-alpha", { exact: true })).toHaveText(
    "No policies",
  );
});

for (const width of [1280, 390]) {
  for (const colorScheme of ["light", "dark"] as const) {
    test(`policies layout ${width} ${colorScheme}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme });
      await page.request.put("/api/policies/settings", {
        data: {
          required: ["workspace", "xolotl-client-key"],
          groups: [{ name: "Operator SSH", mode: "multi", policies: ["bob-ssh", "ira-ssh"] }],
          revision: 0,
        },
      });
      await page.goto("/policies");
      await expect(page.getByRole("textbox", { name: "Group name" })).toHaveValue("Operator SSH");
      await expectNoHorizontalScroll(page);
      await shot(page, `task-22-policies-${width}-${colorScheme}`);
    });
  }
}

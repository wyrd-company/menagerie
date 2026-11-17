import { expect, test } from "@playwright/test";
import { reset, shot, expectNoHorizontalScroll } from "./support.ts";

test.beforeEach(async ({ request }) => {
  await reset(request);
});
for (const width of [1280, 390]) {
  for (const colorScheme of ["light", "dark"] as const) {
    test(`workspace choices ${width} ${colorScheme}`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme });
      const response = await page.request.put("/api/policies/settings", {
        data: {
          required: ["workspace", "xolotl-client-key"],
          groups: [{ name: "Operator SSH", mode: "multi", policies: ["bob-ssh", "ira-ssh"] }],
          revision: 0,
        },
      });
      expect(response.ok()).toBeTruthy();
      await page.goto("/create");
      await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
      await page.getByLabel("Workspace name").fill("sample-policy");
      await page.getByRole("checkbox", { name: "bob-ssh", exact: true }).check();
      await expectNoHorizontalScroll(page);
      await shot(page, `task-24-create-${width}-${colorScheme}`);
      await page.goto("/");
      const row = page
        .getByRole("listitem")
        .filter({ has: page.getByRole("heading", { name: "sample-alpha", exact: true }) });
      await row.getByRole("button", { name: "Policies", exact: true }).click();
      const form = page.getByRole("form", { name: "Policies for sample-alpha" });
      await form.getByRole("checkbox", { name: "bob-ssh", exact: true }).check();
      await form.getByRole("checkbox", { name: "ira-ssh", exact: true }).check();
      await expectNoHorizontalScroll(page);
      await shot(page, `task-24-edit-${width}-${colorScheme}`);
      await form.getByRole("button", { name: "Save", exact: true }).click();
      await expect(row.locator(".entry-policies")).toHaveText(
        "workspace · xolotl-client-key · bob-ssh · ira-ssh",
      );
      await expect(row).toContainText("Policies applied");
    });
  }
}

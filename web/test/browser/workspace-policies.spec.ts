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

for (const [name, message] of [
  [
    "not-offered",
    'ssh-agent.sock does not offer a single usable key for provisioning.wyrwood.operator-key "sample-operator"; check that the Wyrwood agent runs on this host and that Menagerie\'s policy offers that key',
  ],
  [
    "not-approved",
    "the operator key SHA256:sample-pending is not approved (pending, rejected or revoked); approve a pending key in the Wyrwood web interface",
  ],
] as const) {
  test(`create retains the ${name} operator key diagnostic from HTTP 503`, async ({ page }) => {
    await page.route("**/api/jobs", async (route) => {
      expect(route.request().method()).toBe("POST");
      await route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ error: { kind: "wyrwood-unavailable", message } }),
      });
    });
    await page.goto("/create");
    await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
    await page.getByLabel("Workspace name").fill("sample-key-refused");
    await page.getByRole("button", { name: "Begin interview" }).click();
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("wyrwood-unavailable");
    await expect(alert).toContainText(message);
    await expect(page.getByLabel("Workspace name")).toHaveValue("sample-key-refused");
    await expect(page.getByRole("button", { name: "Begin interview" })).toBeEnabled();
    await expect(page).toHaveURL(/\/create$/);
    await expectNoHorizontalScroll(page);
    await shot(page, `task-24-create-key-${name}`);
    // The same key failure can happen before the form has its policy list.
    await page.route("**/api/policies", async (route) => {
      await route.fulfill({
        status: 503,
        contentType: "application/json",
        body: JSON.stringify({ error: { kind: "wyrwood-unavailable", message } }),
      });
    });
    await page.reload();
    await expect(page.getByRole("alert")).toContainText(message);
    await expect(page.getByRole("button", { name: "Retry policies" })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Begin interview" })).toBeDisabled();
    await shot(page, `task-24-create-key-${name}-load`);
  });
}

import { expect, test } from "@playwright/test";
import { expectNoHorizontalScroll, reset, shot } from "./support.ts";

test.beforeEach(async ({ request }) => reset(request));

test("registers a template from its address and creates from it", async ({ page }) => {
  await page.goto("/templates");
  await expect(page.getByRole("heading", { name: "Templates", level: 1 })).toBeVisible();
  await expect(page.getByRole("link", { name: "Templates" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await shot(page, "templates-registered");
  await page.getByRole("button", { name: "Register template" }).first().click();
  await page.getByLabel("Display name").fill("Sample tools");
  await page.getByLabel("Address").fill("https://git.example.test/example-org/sample-tools");
  await page.getByLabel(/Runner image/).fill("alpine:3.22");
  await shot(page, "templates-register");
  await page.getByRole("button", { name: "Save" }).click();
  const entry = page.getByRole("listitem").filter({ hasText: "Sample tools" });
  await expect(entry.getByText("example-org/sample-tools")).toBeVisible();
  await expect(entry.getByText("1.0.0")).toBeVisible();
  await expect(page.getByLabel("Address")).toHaveCount(0);

  await page.getByRole("link", { name: "New workspace" }).click();
  await expect(page.getByRole("radio", { name: /Sample tools/ })).toBeVisible();
  await expect(page.getByRole("radio", { name: /Sample tools 1\.0\.0/ })).toBeVisible();
});

test("names the check a refused template failed, by the address", async ({ page }) => {
  await page.goto("/templates");
  await page.getByRole("button", { name: "Register template" }).first().click();
  await page.getByLabel("Display name").fill("Bare kit");
  await page.getByLabel("Address").fill("https://git.example.test/example-org/bare");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "definition: the template's files at version 1.0.0 have no .devcontainer/devcontainer.json",
  );
  await shot(page, "templates-missing-definition");
  await page.getByLabel("Address").fill("file:///srv/templates/kit");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("alert")).toContainText("address: the address is refused");
  await expect(page.getByRole("alert")).not.toContainText("/srv/templates");
  await shot(page, "templates-address-refused");
  await page.getByLabel("Address").fill("https://git.example.test/example-org/sample-kit");
  await page.getByRole("button", { name: "Save" }).click();
  await expect(page.getByRole("alert")).toContainText("registered already, as Sample kit");
});

test("upgrades a template to a newer version, edits it and refuses its removal while used", async ({
  page,
}) => {
  await page.goto("/templates");
  const entry = page.getByRole("listitem").filter({ hasText: "Sample kit" });
  await entry.getByRole("button", { name: "Upgrade template" }).click();
  await entry.getByLabel("Current version").fill("1.1.0");
  await entry.getByRole("button", { name: "Save" }).click();
  await expect(entry.getByRole("alert")).toContainText(
    "not a version of the template's repository newer",
  );
  await entry.getByLabel("Current version").fill("1.2.0");
  await shot(page, "templates-upgrade");
  await entry.getByRole("button", { name: "Save" }).click();
  await expect(entry.getByText("1.2.0")).toBeVisible();
  await expect(entry.getByLabel("Current version")).toHaveCount(0);

  await entry.getByRole("button", { name: "Edit" }).click();
  await entry.getByLabel("Display name").fill("Sample kit, renamed");
  await entry.getByRole("button", { name: "Save" }).click();
  const renamed = page.getByRole("listitem").filter({ hasText: "Sample kit, renamed" });
  await expect(renamed).toBeVisible();

  await renamed.getByRole("button", { name: "Remove" }).click();
  await expect(renamed.getByRole("alert")).toContainText("used by workspace sample-alpha");
  await shot(page, "templates-remove-refused");
  const tools = page.getByRole("listitem").filter({ hasText: "Tools kit" });
  await tools.getByRole("button", { name: "Remove" }).click();
  await expect(tools).toHaveCount(0);
});

test("an upgrade applies the workspace's template's current version", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("listitem").filter({ hasText: "sample-alpha" })).toContainText(
    "Sample kit",
  );
  await page.goto("/workspaces/sample-alpha/upgrade");
  await expect(page.locator(".upgrade-current")).toContainText("Current template Sample kit 1.1.0");
  await expect(page.getByRole("radio")).toHaveCount(0);
  await shot(page, "upgrade-current-template");
  await page.goto("/workspaces/sample-gone/upgrade");
  await expect(page.getByRole("alert").first()).toBeVisible();
});

test.describe("narrow viewport", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("fits the templates screen", async ({ page }) => {
    await page.goto("/templates");
    await page.getByRole("button", { name: "Register template" }).first().click();
    await expect(page.getByLabel("Address")).toBeVisible();
    await expectNoHorizontalScroll(page);
    await shot(page, "templates-narrow");
  });
});

import { expect, test } from "@playwright/test";
import { control, expectNoHorizontalScroll, reset, shot } from "./support.ts";

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
  const save = page.getByRole("button", { name: "Save" });
  await expect(save).toBeDisabled();
  await page.getByLabel("Display name").fill("Sample tools");
  await page.getByLabel("Address").fill("https://git.example.test/example-org/sample-tools");
  await page.getByLabel(/Runner image/).fill("alpine:3.22");
  await shot(page, "templates-register");
  await expect(save).toBeDisabled();
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.getByRole("status")).toHaveText("Checks passed · 1.0.0");
  await expect(save).toBeEnabled();
  await shot(page, "templates-register-validated");
  // Any edit asks for another Validate.
  await page.getByLabel(/Runner image/).fill("alpine:3.21");
  await expect(save).toBeDisabled();
  await expect(page.getByRole("status")).toHaveCount(0);
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(save).toBeEnabled();
  // The check stored nothing.
  const listed = (await (await page.request.get("/api/templates")).json()) as {
    templates: { name: string }[];
  };
  expect(listed.templates.map((t) => t.name)).not.toContain("Sample tools");
  await save.click();
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
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "definition: the template's files at version 1.0.0 have no .devcontainer/devcontainer.json",
  );
  // Only an address refusal marks the address.
  await expect(page.getByLabel("Address")).not.toHaveAttribute("aria-invalid", "true");
  await expect(page.getByRole("button", { name: "Save" })).toBeDisabled();
  await shot(page, "templates-missing-definition");
  await page.getByLabel("Address").fill("file:///srv/templates/kit");
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.getByRole("alert")).toContainText("address: the address is refused");
  await expect(page.getByRole("alert")).not.toContainText("/srv/templates");
  await expect(page.getByLabel("Address")).toHaveAttribute("aria-invalid", "true");
  await shot(page, "templates-address-refused");
  await page.getByLabel("Address").fill("https://git.example.test/example-org/sample-kit");
  await page.getByRole("button", { name: "Validate" }).click();
  await expect(page.getByRole("alert")).toContainText("registered already, as Sample kit");
});

test("upgrades a template to a newer found version, edits it and refuses its removal while used", async ({
  page,
}) => {
  await page.goto("/templates");
  const entry = page.getByRole("listitem").filter({ hasText: "Sample kit" });
  await expect(entry).toContainText("Checked 1 hour ago");
  await expect(entry.locator(".template-versions")).toContainText("Current 1.1.0");
  await expect(entry.locator(".template-versions")).toContainText("Newest 1.2.0");
  await expect(entry.getByText("New version")).toBeVisible();
  await entry.getByRole("button", { name: "Upgrade template" }).click();
  await expect(entry.getByRole("button", { name: "Save" })).toBeDisabled();
  // Only the found versions newer than the current one are offered.
  await expect(entry.getByLabel("Current version").locator("option")).toHaveText(["1.2.0"]);
  await shot(page, "templates-upgrade");
  await entry.getByRole("button", { name: "Validate" }).click();
  await expect(entry.getByRole("status")).toHaveText("Checks passed · 1.2.0");
  await expect(entry.locator(".template-versions")).toContainText("Current 1.1.0");
  await shot(page, "templates-upgrade-validated");
  await entry.getByRole("button", { name: "Save" }).click();
  await expect(entry.locator(".template-versions")).toContainText("Current 1.2.0");
  await expect(entry.getByLabel("Current version")).toHaveCount(0);
  // No newer version is found: no template upgrade is offered.
  await expect(entry.getByRole("button", { name: "Upgrade template" })).toHaveCount(0);
  await expect(entry.getByText("New version")).toHaveCount(0);

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

test("finds a new version on request, shows a failed check and takes a moved tag again", async ({
  page,
  request,
}) => {
  await page.goto("/templates");
  const entry = page.getByRole("listitem").filter({ hasText: "Tools kit" });
  await expect(entry.getByRole("button", { name: "Upgrade template" })).toHaveCount(0);
  // The repository gets a tag; with no restart, a check on request finds it.
  await control(request, "release-version", { template: "tools-kit", version: "2.1.0" });
  await control(request, "hold-checks", { hold: true });
  await entry.getByRole("button", { name: "Check versions" }).click();
  await expect(entry).toContainText("Checking versions…");
  await expect(entry.getByRole("button", { name: "Check versions" })).toBeDisabled();
  await shot(page, "templates-checking");
  await control(request, "hold-checks", { hold: false });
  await expect(entry).toContainText("Checked just now");
  await expect(entry.locator(".template-versions")).toContainText("Newest 2.1.0");
  await expect(entry.getByText("New version")).toBeVisible();
  await shot(page, "templates-newer");
  // A check while the template service is down keeps what was found.
  await control(request, "template-service", { available: false });
  await entry.getByRole("button", { name: "Check versions" }).click();
  await expect(entry.locator(".template-check")).toHaveText(
    "Check failed just now: template service is unavailable. Previously found versions are kept.",
  );
  await expect(entry.locator(".template-versions")).toContainText("Newest 2.1.0");
  await shot(page, "templates-check-failed");
  await control(request, "template-service", { available: true });
  await entry.getByRole("button", { name: "Upgrade template" }).click();
  await entry.getByRole("button", { name: "Validate" }).click();
  await entry.getByRole("button", { name: "Save" }).click();
  await expect(entry.locator(".template-versions")).toContainText("Current 2.1.0");
  // The current version's tag moves to another commit.
  await control(request, "move-tag", { template: "tools-kit" });
  await entry.getByRole("button", { name: "Check versions" }).click();
  await expect(entry).toContainText(
    "Current tag moved. Validate and save this version again before using it.",
  );
  await expect(entry.getByText("Tag moved")).toBeVisible();
  await expect(entry.getByRole("button", { name: "Upgrade template" })).toHaveCount(0);
  await entry.getByRole("button", { name: "Update version" }).click();
  await expect(entry.getByLabel("Current version").locator("option")).toHaveText(["2.1.0"]);
  await shot(page, "templates-moved");
  await entry.getByRole("button", { name: "Validate" }).click();
  await expect(entry.getByRole("status")).toHaveText("Checks passed · 2.1.0");
  await entry.getByRole("button", { name: "Save" }).click();
  await expect(entry.getByText("Tag moved")).toHaveCount(0);
  await expect(entry.getByRole("button", { name: "Update version" })).toHaveCount(0);
});

test("refuses an upgrade from a moved tag and shows each workspace's applied version", async ({
  page,
  request,
}) => {
  await control(request, "applied", { workspace: "sample-gamma", version: null, commit: "c0ffee" });
  await control(request, "applied", { workspace: "sample-beta", version: "1.1.0" });
  await page.goto("/");
  const alpha = page.getByRole("listitem").filter({ hasText: "sample-alpha" });
  await expect(alpha.locator(".entry-template")).toHaveText("Sample kit · 1.0.0 applied");
  await expect(alpha.locator(".entry-upgrade")).toHaveText("Needs upgrade · 1.1.0 current");
  await expect(alpha.getByRole("link", { name: "Upgrade" })).toBeVisible();
  const beta = page.getByRole("listitem").filter({ hasText: "sample-beta" });
  await expect(beta.locator(".entry-template")).toHaveText("Sample kit · 1.1.0 applied");
  await expect(beta.locator(".entry-upgrade")).toHaveCount(0);
  await expect(beta.getByText("Upgrade", { exact: true })).toHaveAttribute("aria-disabled", "true");
  const gamma = page.getByRole("listitem").filter({ hasText: "sample-gamma" });
  await expect(gamma.locator(".entry-template")).toHaveText("Sample kit · Unknown version");
  await expect(gamma.locator(".entry-template")).toHaveAttribute("title", "Applied commit c0ffee");
  await expect(gamma.getByRole("link", { name: "Upgrade" })).toBeVisible();
  await shot(page, "workspaces-versions");
  await control(request, "move-tag", { template: "sample-kit" });
  await control(request, "template", { template: "sample-kit", set: { currentMoved: true } });
  await page.goto("/workspaces/sample-alpha/upgrade");
  await expect(page.getByRole("alert")).toHaveText(
    "The current tag moved. Validate and save the version on Templates before upgrading.",
  );
  await expect(page.getByRole("button", { name: "Begin upgrade" })).toBeDisabled();
  await shot(page, "upgrade-moved");
});

test("an upgrade applies the workspace's template's current version", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("listitem").filter({ hasText: "sample-alpha" })).toContainText(
    "Sample kit",
  );
  await page.goto("/workspaces/sample-alpha/upgrade");
  await expect(page.locator(".upgrade-current strong").first()).toHaveText("1.0.0");
  await expect(page.locator(".upgrade-current strong").last()).toHaveText("Sample kit 1.1.0");
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

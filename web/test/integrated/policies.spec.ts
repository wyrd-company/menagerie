// #88: real browser -> Menagerie -> released Wyrwood and Xolotl. This runs
// after the existing acceptance story, on its prepared provisioning host.
import { expect, test } from "@playwright/test";
import { console_, containerFor, control, entry, info, shot, signIn } from "./support.ts";

test("choose at create, edit immediately, and retain choices across an upgrade", async ({
  page,
}) => {
  await signIn(page, "sample-manager");
  const headers = { Origin: info.site, "Sec-Fetch-Site": "same-origin" };
  const catalog = await (await page.request.get(info.site + "/api/policies")).json();
  const settings = await page.request.put(info.site + "/api/policies/settings", {
    headers,
    data: {
      revision: catalog.settings.revision,
      required: ["workspace", "xolotl-client-key"],
      groups: [{ name: "Operator SSH", mode: "multi", policies: ["bob-ssh", "ira-ssh"] }],
    },
  });
  expect(settings.ok()).toBeTruthy();
  const invalid = await page.request.post(info.site + "/api/jobs", {
    headers,
    data: {
      operation: "create",
      workspace: "policy-missing-choice",
      templateId: "workspace-kit",
      policies: [],
    },
  });
  expect(invalid.status()).toBe(422);
  expect((await invalid.json()).error.kind).toBe("policy-choice-required");
  await page.goto(info.site + "/create");
  await page.getByRole("radio", { name: /Workspace kit/ }).check();
  await page.getByLabel("Workspace name").fill("policy-space");
  await page.getByRole("checkbox", { name: "bob-ssh", exact: true }).check();
  await shot(page, "policy-create-choices");
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.locator("[data-question]")).toHaveCount(4);
  await page.getByLabel(/^Label/).fill("Policy workspace");
  await page.getByRole("group", { name: /^Size/ }).getByRole("radio", { name: "large" }).check();
  await page.getByLabel(/^Label/).press("Control+Enter");
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  const before = await containerFor(page.request, "policy-space");
  expect(before?.state).toBe("running");
  const createdBinding = (await control(
    page.request,
    "GET",
    "/binding?name=devcontainer-policy-space",
  )) as { Policies: string[] };
  expect(createdBinding.Policies.sort()).toEqual(["bob-ssh", "workspace", "xolotl-client-key"]);
  await page.goto(info.site + "/");
  await entry(page, "policy-space").getByRole("button", { name: "Policies", exact: true }).click();
  const panel = page.getByRole("form", { name: "Policies for policy-space" });
  await panel.getByRole("checkbox", { name: "ira-ssh", exact: true }).check();
  await panel.getByRole("button", { name: "Save", exact: true }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  const binding = (await control(
    page.request,
    "GET",
    "/binding?name=devcontainer-policy-space",
  )) as { Policies: string[] };
  expect(binding.Policies.sort()).toEqual(["bob-ssh", "ira-ssh", "workspace", "xolotl-client-key"]);
  expect((await containerFor(page.request, "policy-space"))?.id).toBe(before?.id);
  await shot(page, "policy-edited-no-rebuild");
  // The workspace kit's repository also has 2.0.0, which a version check
  // finds: its next current version.
  const checked = await page.request.post(
    info.site + "/api/templates/workspace-kit/version-checks",
    { headers },
  );
  expect(checked.status()).toBe(200);
  expect(await checked.json()).toMatchObject({ newestVersion: "2.0.0" });
  const upgraded = await page.request.post(info.site + "/api/templates/workspace-kit/upgrades", {
    headers,
    data: { version: "2.0.0" },
  });
  expect(upgraded.status()).toBe(200);
  await page.goto(info.site + "/workspaces/policy-space/upgrade");
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await page.getByRole("radio", { name: "north" }).check();
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(console_(page).locator(".outcome")).toContainText("Completed.");
  const after = (await control(page.request, "GET", "/binding?name=devcontainer-policy-space")) as {
    Policies: string[];
  };
  expect(after.Policies.sort()).toEqual(binding.Policies.sort());
  await page.goto(info.site + "/");
  await expect(entry(page, "policy-space")).toContainText("Policies applied");
  await shot(page, "policy-upgrade-keeps-selection");
});

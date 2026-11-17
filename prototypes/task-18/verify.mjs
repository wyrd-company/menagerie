// Standalone mockup acceptance, not product tests. Install Playwright and axe in a disposable environment.
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createRequire } from "node:module";
const require = createRequire(
  path.resolve(
    process.env.MOCKUP_TEST_DEPS || "/tmp/mockup-check/package.json",
  ),
);
const { chromium } = require("playwright");
const { default: AxeBuilder } = require("@axe-core/playwright");
const base = process.argv[2] || "http://127.0.0.1:8080/";
const out = process.argv[3] || "/home/user/mockup/evidence";
await fs.mkdir(out, { recursive: true });
const report = {
  status: "FAILED",
  base,
  started: new Date().toISOString(),
  shots: [],
  flows: [],
  errors: [],
  accessibility: [],
};
const browser = await chromium.launch();
const context = await browser.newContext();
const page = await context.newPage();
page.on("pageerror", (e) => report.errors.push(e.message));
page.on("console", (m) => {
  if (m.type() === "error") report.errors.push(m.text());
});
const fixture = async (value) => {
  await page.locator("#scene").selectOption(value);
  await page.waitForTimeout(30);
};
const action = (name, root = page) => root.locator(`[data-action="${name}"]`);
const navigate = async (hash) => {
  await page.goto(base + "#" + hash);
  await page.waitForTimeout(50);
};
const check = async (name, fn) => {
  await fn();
  report.flows.push(name);
  console.log("PASS " + name);
};
try {
  await page.goto(base);
  const scenes = await page
    .locator("#scene option")
    .evaluateAll((options) => options.map((o) => o.value));
  for (const width of [1280, 390])
    for (const theme of ["light", "dark"]) {
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
      for (const scene of scenes) {
        await fixture(scene);
        await page.evaluate(() => document.fonts.ready);
        assert.equal(
          await page.locator("h1").count(),
          1,
          scene + " has one h1",
        );
        assert.equal(
          await page.evaluate(
            () => document.documentElement.scrollWidth > window.innerWidth,
          ),
          false,
          scene + " has no horizontal overflow at " + width,
        );
        assert.equal(
          await page
            .locator(
              'button:has-text("Review changes"), button:has-text("Cancel")',
            )
            .count(),
          0,
        );
        if (width === 1280 && theme === "light") {
          const result = await new AxeBuilder({ page })
            .withTags(["wcag2a", "wcag2aa", "wcag21aa", "wcag22aa"])
            .analyze();
          report.accessibility.push({
            scene,
            violations: result.violations.map((v) => ({
              id: v.id,
              impact: v.impact,
              nodes: v.nodes.map((n) => ({
                target: n.target,
                summary: n.failureSummary,
              })),
            })),
          });
        }
        const file =
          scene.replaceAll("/", "-") + "-" + width + "-" + theme + ".png";
        await page.screenshot({ path: path.join(out, file), fullPage: true });
        report.shots.push(file);
      }
    }
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.emulateMedia({ colorScheme: "light" });
  await check(
    "#87 register, create at 1.0.0, discover 1.1.0, advance template, upgrade workspace",
    async () => {
      await fixture("templates/empty");
      await action("open-register").first().click();
      await page.locator("#template-name").fill("Workspace");
      await page
        .locator("#template-address")
        .fill("https://github.com/example/workspace-template");
      await action("validate-registration").click();
      await action("save-registration").click();
      await page
        .getByRole("link", { name: "New workspace", exact: true })
        .click();
      await page.locator("#workspace-name").fill("acceptance-workspace");
      await page.locator('[data-context="new"][value="bob-ssh"]').check();
      await page.locator('[data-context="new"][value="development"]').check();
      await page.getByRole("button", { name: "Begin interview" }).click();
      await page.getByRole("button", { name: "Submit answers" }).click();
      await page.locator(".console").waitFor();
      await page
        .getByRole("link", { name: "Back to the workspace list" })
        .click();
      let row = page.locator(".entry").filter({
        has: page.getByRole("heading", {
          name: "acceptance-workspace",
          exact: true,
        }),
      });
      assert.match(await row.textContent(), /1.0.0 applied/);
      await page.getByRole("link", { name: "Templates", exact: true }).click();
      await action("check-versions").click();
      await page.waitForFunction(() =>
        document
          .querySelector(".template-versions")
          .textContent.includes("1.1.0"),
      );
      await action("template-upgrade").click();
      await action("validate-version").click();
      await action("save-version").click();
      await page.getByRole("link", { name: "Workspaces", exact: true }).click();
      row = page.locator(".entry").filter({
        has: page.getByRole("heading", {
          name: "acceptance-workspace",
          exact: true,
        }),
      });
      assert.match(await row.textContent(), /Needs upgrade/);
      await row.getByRole("link", { name: "Upgrade", exact: true }).click();
      await page.getByRole("button", { name: "Begin upgrade" }).click();
      await page.getByRole("button", { name: "Submit answers" }).click();
      await page.locator(".console").waitFor();
      assert.match(
        await page.locator("main").textContent(),
        /policies unchanged/,
      );
      await page
        .getByRole("link", { name: "Back to the workspace list" })
        .click();
      assert.match(await row.textContent(), /1.1.0 applied/);
      assert.doesNotMatch(await row.textContent(), /Needs upgrade/);
    },
  );
  await check(
    "#88 missing group refused, choose bob-ssh, create, add ira-ssh, remove ordinary policy",
    async () => {
      await fixture("new");
      await page.getByRole("button", { name: "Begin interview" }).click();
      assert.match(
        await page.locator("#create-error").textContent(),
        /Operator SSH/,
      );
      await page.locator('[data-context="new"][value="bob-ssh"]').check();
      await page.locator('[data-context="new"][value="development"]').check();
      await page.getByRole("button", { name: "Begin interview" }).click();
      await page.getByRole("button", { name: "Submit answers" }).click();
      await page.locator(".console").waitFor();
      await page
        .getByRole("link", { name: "Back to the workspace list" })
        .click();
      const row = page.locator(".entry").filter({
        has: page.getByRole("heading", { name: "sample-new", exact: true }),
      });
      await action("toggle-policies", row).click();
      await row.locator('[value="ira-ssh"]').check();
      await action("save-selection", row).click();
      assert.match(
        await row.textContent(),
        /applied workspace, xolotl-client-key, bob-ssh, development, ira-ssh/,
      );
      await row.locator('[value="bob-ssh"]').uncheck();
      await action("save-selection", row).click();
      assert.match(
        await row.textContent(),
        /applied workspace, xolotl-client-key, development, ira-ssh/,
      );
      assert.equal(await row.locator('[value="workspace"]').isDisabled(), true);
    },
  );
  await check(
    "Newly missing group permits an unrelated edit and keeps needs-choice warning",
    async () => {
      await fixture("workspaces/needs-choice");
      const row = page.locator(".entry").first();
      await row.locator('[value="registry-read"]').check();
      await action("save-selection", row).click();
      assert.match(await row.textContent(), /Policies applied/);
      assert.match(await row.textContent(), /Needs a choice: Environment/);
    },
  );
  await check(
    "Outside-grant create refused before interview; selected outside-grant policy remains removable",
    async () => {
      await fixture("new/outside-grant");
      await page.getByRole("button", { name: "Begin interview" }).click();
      assert.match(
        await page.locator("#create-error").textContent(),
        /registry-read.*No job was started/,
      );
      await fixture("workspaces/outside-grant");
      const row = page.locator(".entry").first();
      await row.locator('[value="legacy-admin"]').uncheck();
      await action("save-selection", row).click();
      assert.match(await row.textContent(), /Policies applied/);
    },
  );
  await check(
    "Single-select switches; removing the final valid group choice is refused",
    async () => {
      await fixture("workspaces");
      const row = page.locator(".entry").first();
      await action("toggle-policies", row).click();
      await row.locator('[value="production"]').check();
      assert.equal(
        await row.locator('[value="development"]').isChecked(),
        false,
      );
      await row.locator('[value="bob-ssh"]').uncheck();
      await action("save-selection", row).click();
      assert.match(
        await row.locator('[role="alert"]').textContent(),
        /Choose at least one policy in Operator SSH/,
      );
    },
  );
  await check(
    "Empty selection saved and sent to Wyrwood; refusal preserves binding; unbound empty applies",
    async () => {
      await fixture("policies/empty");
      await page.getByRole("link", { name: "Workspaces", exact: true }).click();
      const row = page.locator(".entry").first();
      await action("toggle-policies", row).click();
      for (const p of [
        "workspace",
        "xolotl-client-key",
        "bob-ssh",
        "development",
      ])
        await row.locator(`[value="${p}"]`).uncheck();
      await action("save-selection", row).click();
      assert.match(await row.textContent(), /Your empty selection is saved/);
      assert.match(await row.textContent(), /empty selection sent to Wyrwood/);
      assert.match(await row.textContent(), /Binding policies: workspace/);
      await fixture("workspaces/empty-unbound");
      await action("save-selection", page.locator(".entry").first()).click();
      assert.match(
        await page.locator(".entry").first().textContent(),
        /no current container name; no binding request/,
      );
    },
  );
  await check("Pending and failed applies can be retried", async () => {
    for (const state of ["pending", "failed"]) {
      await fixture("workspaces/" + state);
      const row = page.locator(".entry").first();
      await action("apply-again", row).click();
      assert.match(await row.textContent(), /Policies applied/);
    }
  });
  await check(
    "Template registration missing definition; used removal refused; unused removal succeeds; moved version revalidates",
    async () => {
      await fixture("templates/register");
      await page
        .locator("#template-address")
        .fill("https://github.com/example/missing-definition");
      await action("validate-registration").click();
      assert.match(
        await page.locator("#registration-error").textContent(),
        /\.devcontainer\/devcontainer.json/,
      );
      assert.equal(await action("save-registration").isDisabled(), true);
      await action("remove-template").first().click();
      assert.match(
        await page.locator("main").textContent(),
        /Cannot remove Workspace/,
      );
      await action("remove-template").last().click();
      assert.equal(await page.locator(".template-entry").count(), 1);
      await fixture("templates/moved");
      await action("validate-version").click();
      await action("save-version").click();
      assert.doesNotMatch(
        await page.locator("main").textContent(),
        /Current tag moved/,
      );
    },
  );
  await check(
    "Required policy applies fleet-wide; unmarking retains workspace selections; add/edit/remove group",
    async () => {
      await fixture("policies");
      await page.locator('[data-required="registry-read"]').check();
      await page.getByRole("button", { name: "Save", exact: true }).click();
      await page.getByRole("link", { name: "Workspaces", exact: true }).click();
      await action("toggle-policies", page.locator(".entry").first()).click();
      assert.equal(
        await page
          .locator(".entry")
          .first()
          .locator('[value="registry-read"]')
          .isChecked(),
        true,
      );
      await page.getByRole("link", { name: "Policies", exact: true }).click();
      await page.locator('[data-required="registry-read"]').uncheck();
      await page.getByRole("button", { name: "Save", exact: true }).click();
      await page.getByRole("link", { name: "Workspaces", exact: true }).click();
      assert.equal(
        await page
          .locator(".entry")
          .first()
          .locator('[value="registry-read"]')
          .isChecked(),
        true,
      );
      await page.getByRole("link", { name: "Policies", exact: true }).click();
      await action("add-group").click();
      const group = page.locator('[data-group="new"]');
      await group.locator("[data-group-name]").fill("Registry");
      await group.locator('[data-group-member="registry-read"]').check();
      await page.getByRole("button", { name: "Save", exact: true }).click();
      assert.equal(await page.locator("[data-group-name]").count(), 3);
      await action("remove-group").last().click();
      await page.getByRole("button", { name: "Save", exact: true }).click();
      assert.equal(await page.locator("[data-group-name]").count(), 2);
    },
  );
  await check(
    "Ask every question again includes the recorded title",
    async () => {
      await fixture("upgrade/sample-space");
      await page.locator("#reanswer").check();
      await page.getByRole("button", { name: "Begin upgrade" }).click();
      await page.locator("#previous-title").waitFor();
      assert.equal(
        await page.locator("#previous-title").inputValue(),
        "Shared workspace",
      );
      assert.equal(await page.locator("#answer").count(), 1);
    },
  );
  await check(
    "Keyboard navigation can reach primary controls with visible focus",
    async () => {
      await fixture("templates/register");
      await page.locator("#template-name").focus();
      await page.keyboard.press("Tab");
      assert.equal(
        await page
          .locator("#template-address")
          .evaluate((el) => el === document.activeElement),
        true,
      );
      await page.keyboard.press("Tab");
      await page.keyboard.press("Tab");
      assert.equal(
        await action("validate-registration").evaluate(
          (el) => el === document.activeElement,
        ),
        true,
      );
      assert.notEqual(
        await action("validate-registration").evaluate(
          (el) => getComputedStyle(el).boxShadow,
        ),
        "none",
      );
    },
  );
  const violations = report.accessibility.flatMap((a) =>
    a.violations.map((v) => ({ scene: a.scene, ...v })),
  );
  assert.deepEqual(report.errors, [], "No browser errors");
  assert.deepEqual(violations, [], "No WCAG A/AA scanner violations");
  report.status = "PASSED";
  report.finished = new Date().toISOString();
  console.log(
    `PASS ${report.shots.length} rendered states, ${report.flows.length} flows, ${report.accessibility.length} axe scans`,
  );
} catch (e) {
  report.failure = e.stack;
  console.error(e);
  process.exitCode = 1;
} finally {
  await fs.writeFile(
    path.join(out, "report.json"),
    JSON.stringify(report, null, 2),
  );
  await browser.close();
}

// The web UI against the real workspace service, template service, Docker
// Engine, Git backup host and identity provider (see run.sh). One serial
// story: each step builds on the workspace the previous step left.
import { expect, test, type Page } from "@playwright/test";
import {
  backup,
  console_,
  containerFor,
  control,
  entry,
  finished,
  info,
  ownPage,
  shot,
  signIn,
} from "./support.ts";

test.describe.configure({ mode: "serial" });

test("a signed-in person who is not a manager is refused", async ({ page }) => {
  await signIn(page, "sample-other");
  await expect(page.locator("body")).toContainText("Sample Other");
  await expect(page.locator('[data-kind="not-a-manager"]')).toBeVisible();
  const api = await page.request.get(info.site + "/api/workspaces");
  expect(api.status()).toBe(401);
  await shot(page, "01-not-a-manager");
});

test.describe("as a manager", () => {
  let page: Page;
  test.beforeAll(async ({ browser }) => {
    page = await ownPage(browser);
    await signIn(page, "sample-manager");
    await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  });
  test.afterAll(() => page.close());

  test("lists folders and never an unrelated container", async () => {
    await control(page.request, "POST", "/folder?name=sample-folder");
    await control(page.request, "POST", "/outsider");
    const outsider = (await control(page.request, "GET", "/containers")) as {
      state: string;
      folder: string;
    }[];
    expect(outsider.find((c) => c.folder === "/elsewhere/sample-outsider")?.state).toBe("running");
    await page.getByRole("button", { name: "Refresh" }).click();
    await expect(entry(page, "sample-folder").locator(".status")).toHaveText("No container");
    await expect(page.getByRole("heading", { name: "sample-outsider" })).toHaveCount(0);
    await shot(page, "02-list-folder");
  });

  test("creates a workspace through its interview and starts its devcontainer", async () => {
    await page.getByRole("link", { name: "New workspace" }).first().click();
    await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
    await page.getByLabel("Workspace name").fill("sample-space");
    await page.getByRole("button", { name: "Begin interview" }).click();
    await expect(page.locator("[data-question]")).toHaveCount(4);
    await shot(page, "03-create-interview");
    await page.getByLabel(/^Label/).fill("Sample project");
    await page.getByRole("group", { name: /^Size/ }).getByRole("radio", { name: "large" }).check();
    await page.getByLabel(/^Label/).press("Control+Enter");
    await expect(console_(page).locator(".outcome")).toContainText("Completed.");
    await expect(console_(page).getByRole("list", { name: "Phases" })).toHaveText(
      /interview.*initialize-repository.*generate.*commit.*backup.*initialize-command.*up/s,
    );
    await expect(console_(page).locator(".apply")).toContainText("written");
    await shot(page, "04-create-completed");

    const container = await containerFor(page.request, "sample-space");
    expect(container?.state).toBe("running");
    const branch = await backup(page.request, "sample-space");
    expect(branch.commits).toHaveLength(1);
    expect(branch.commits[0]!.parents).toBe(0);
    expect(branch.files["result.txt"]).toBe("label: Sample project\nsize: large\n");
    expect(branch.files[".toha.json"]).toBeDefined();
    expect(branch.files["hook-marker.txt"]).toBe("ran\n");
    expect(branch.files["markers/initialize.txt"]).toBeUndefined();
    const marker = await control(
      page.request,
      "GET",
      "/file?workspace=sample-space&path=markers/initialize.txt",
    );
    expect(marker).toMatchObject({ exists: true, body: "initialized\n" });

    await page.getByRole("link", { name: "Back to the workspace list" }).click();
    await expect(entry(page, "sample-space").locator(".status")).toHaveText("Running");
  });

  test("stops, starts and rebuilds the workspace", async () => {
    const before = await containerFor(page.request, "sample-space");
    await entry(page, "sample-space").getByRole("button", { name: "Stop" }).click();
    await finished(page, /Stop sample-space/);
    await expect(entry(page, "sample-space").locator(".status")).toHaveText("Stopped");
    await entry(page, "sample-space").getByRole("button", { name: "Start" }).click();
    await finished(page, /Start sample-space/);
    await expect(entry(page, "sample-space").locator(".status")).toHaveText("Running");
    await entry(page, "sample-space").getByRole("button", { name: "Rebuild" }).click();
    await entry(page, "sample-space").getByRole("button", { name: "Confirm rebuild" }).click();
    await finished(page, /Rebuild sample-space/);
    await expect(entry(page, "sample-space").locator(".status")).toHaveText("Running");
    const after = await containerFor(page.request, "sample-space");
    expect(after?.state).toBe("running");
    expect(after?.id).not.toBe(before?.id);
    await shot(page, "05-list-after-rebuild");
  });

  test("upgrades with only the new question and keeps an operator edit", async () => {
    const local = await control(page.request, "GET", "/file?workspace=sample-space&path=local.txt");
    await control(
      page.request,
      "PUT",
      "/file?workspace=sample-space&path=local.txt",
      local.body.replace("second: template", "second: operator"),
    );
    await entry(page, "sample-space").getByRole("link", { name: "Upgrade" }).click();
    await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
    await page.getByRole("button", { name: "Begin upgrade" }).click();
    await expect(page.locator("[data-question]")).toHaveCount(1);
    await expect(page.locator('[data-question="region"]')).toBeVisible();
    await shot(page, "06-upgrade-interview");
    await page.getByRole("radio", { name: "north" }).check();
    await page.getByRole("button", { name: "Send answers" }).click();
    await expect(console_(page).locator(".outcome")).toContainText("Completed.");
    await expect(console_(page).locator(".apply")).toContainText("written");
    await shot(page, "07-upgrade-completed");

    const branch = await backup(page.request, "sample-space");
    expect(branch.commits.length).toBeGreaterThanOrEqual(3);
    expect(branch.files["local.txt"]).toContain("second: operator");
    expect(branch.files["local.txt"]).toContain("fifth: v2");
    expect(branch.files["result.txt"]).toBe("label: Sample project\nsize: large\nregion: north\n");
    expect((await containerFor(page.request, "sample-space"))?.state).toBe("running");
  });

  test("a 401 from the event stream sends the person to sign in", async () => {
    const jobs = (await (await page.request.get(info.site + "/api/jobs")).json()) as {
      jobs: { jobId: string }[];
    };
    const jobId = jobs.jobs[0]!.jobId;
    await page.goto("/");
    const link = page
      .getByRole("region", { name: "Jobs since Menagerie started" })
      .locator(`a[href="/?job=${encodeURIComponent(jobId)}"]`);
    // The list's reads finish with the session, or they meet the 401 before the stream does.
    await expect(link).toBeVisible();
    await page.context().clearCookies({ domain: "localhost", name: undefined });
    const events = page.waitForResponse((r) => r.url().includes(`/api/jobs/${jobId}/events`));
    // The job's summary read meets the 401 too and redirects; held until the stream
    // answers, that redirect cannot cancel the stream's request.
    const summary = (u: URL) => u.pathname === `/api/jobs/${jobId}`;
    await page.route(summary, async (route) => {
      await events;
      await route.continue();
    });
    // An in-app navigation: no page request, so the API and the stream meet the missing session.
    await link.click();
    expect((await events).status()).toBe(401);
    // Each 401 starts a redirect and a later one aborts an earlier one's navigation, which
    // fails waitForURL and toHaveURL; polling the URL sees only the navigation that commits.
    await expect.poll(() => new URL(page.url()).origin).toBe(new URL(info.pocket).origin);
    await page.unroute(summary);
    await signIn(page, "sample-manager");
  });

  test("signs out through the UI's own form", async () => {
    await page.goto("/");
    const logout = page.waitForRequest((r) => r.url().endsWith("/auth/logout"));
    await page.getByRole("button", { name: "Sign out" }).click();
    expect((await logout).method()).toBe("POST");
    await page.waitForURL((u) => u.pathname === "/auth/signed-out");
    expect((await page.request.get(info.site + "/api/workspaces")).status()).toBe(401);
    await shot(page, "08-signed-out");
  });
});

// The built image's failures and restart, on the workspaces the integrated
// story left (see acceptance.sh). Each failure is produced where it starts:
// the identity provider's groups, Toha's answer rules, initializeCommand,
// the workspace repository, a reviewed branch pushed to it, a template hook
// held on a release file, and a restart of the container.
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

type Snapshot = {
  jobId: string;
  workspace: string;
  operation: string;
  state: string;
  error: { kind: string; message: string } | null;
};

/** The newest job of an operation on a workspace. */
async function newestJob(page: Page, workspace: string, operation: string) {
  const { jobs } = (await (await page.request.get(info.site + "/api/jobs")).json()) as {
    jobs: Snapshot[];
  };
  const job = jobs.find((j) => j.workspace === workspace && j.operation === operation);
  expect(job, `a ${operation} job for ${workspace}`).toBeDefined();
  return job!;
}

/**
 * Posts a job with the page's session from the site's origin. The refusal
 * page's policy forbids scripts to fetch, so the request does not come
 * from the page itself.
 */
const postJob = async (page: Page, body: Record<string, string>) =>
  (
    await page.request.post(info.site + "/api/jobs", {
      data: body,
      headers: { Origin: info.site, "Sec-Fetch-Site": "same-origin" },
    })
  ).status();

const folder = async (page: Page, name: string) =>
  ((await control(page.request, "GET", `/folder?name=${name}`)) as { exists: boolean }).exists;

test("a non-manager's job requests change nothing", async ({ page }) => {
  await signIn(page, "sample-other");
  await expect(page.locator('[data-kind="not-a-manager"]')).toBeVisible();
  const before = await containerFor(page.request, "sample-space");
  expect(before?.state).toBe("running");
  expect(await postJob(page, { operation: "stop", workspace: "sample-space" })).toBe(401);
  expect(
    await postJob(page, {
      operation: "create",
      workspace: "denied-space",
      templateId: "sample-kit-v1",
    }),
  ).toBe(401);
  expect(await containerFor(page.request, "sample-space")).toEqual(before);
  expect(await folder(page, "denied-space")).toBe(false);
});

test.describe("as a manager", () => {
  let page: Page;
  test.beforeAll(async ({ browser }) => {
    page = await ownPage(browser);
    await signIn(page, "sample-manager");
    await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
  });
  test.afterAll(() => page.close());

  test("an invalid answer stays in the form to correct", async () => {
    await page.goto("/");
    await page.getByRole("link", { name: "New workspace" }).first().click();
    await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
    await page.getByLabel("Workspace name").fill("kept-space");
    await page.getByRole("button", { name: "Begin interview" }).click();
    await expect(page.locator("[data-question]")).toHaveCount(4);
    // Toha's rule: a label is 3 to 40 characters.
    await page.getByLabel(/^Label/).fill("ab");
    await page.getByLabel(/^Label/).press("Control+Enter");
    await expect(page.getByLabel(/^Label/)).toHaveAttribute("aria-invalid", "true");
    await expect(page.getByLabel(/^Label/)).toHaveValue("ab");
    await shot(page, "09-invalid-answer");
    expect(await folder(page, "kept-space")).toBe(false);
    await page.getByLabel(/^Label/).fill("Kept project");
    await page.getByLabel(/^Label/).press("Control+Enter");
    await expect(console_(page).locator(".outcome")).toContainText("Completed.");
    expect((await containerFor(page.request, "kept-space"))?.state).toBe("running");
  });

  test("a failing initializeCommand keeps the devcontainer stopped", async () => {
    await page.goto("/");
    await entry(page, "kept-space").getByRole("button", { name: "Stop" }).click();
    await finished(page, /Stop kept-space/);
    const path = "/file?workspace=kept-space&path=.devcontainer/devcontainer.json";
    const original = ((await control(page.request, "GET", path)) as { body: string }).body;
    const failing = original.replace(
      "mkdir -p markers && echo initialized > markers/initialize.txt",
      "echo sample initialize failure >&2; exit 3",
    );
    expect(failing).not.toBe(original);
    await control(page.request, "PUT", path, failing);
    try {
      await entry(page, "kept-space").getByRole("button", { name: "Start" }).click();
      await finished(page, /Start kept-space/, "Failed.");
      await expect(console_(page).locator(".outcome-error")).toContainText(
        "initialize-command-failed",
      );
      await shot(page, "10-initialize-failed");
      expect((await containerFor(page.request, "kept-space"))?.state).not.toBe("running");
      expect((await newestJob(page, "kept-space", "start")).error?.kind).toBe(
        "initialize-command-failed",
      );
    } finally {
      await control(page.request, "PUT", path, original);
    }
    await entry(page, "kept-space").getByRole("button", { name: "Start" }).click();
    await finished(page, /Start kept-space/);
    expect((await containerFor(page.request, "kept-space"))?.state).toBe("running");
  });

  test("a refused backup is a failed upgrade that changes nothing", async () => {
    const before = await backup(page.request, "kept-space");
    await control(page.request, "POST", "/backup/break");
    try {
      await page.goto("/");
      await entry(page, "kept-space").getByRole("link", { name: "Upgrade" }).click();
      await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
      // The upgrade backs the workspace up before it asks anything.
      await page.getByRole("button", { name: "Begin upgrade" }).click();
      await expect(console_(page).locator(".outcome")).toContainText("Failed.");
      await expect(page.locator("[data-question]")).toHaveCount(0);
      await expect(console_(page).locator(".outcome-error")).toContainText(
        "backup-failed backing up the workspace before the upgrade failed",
      );
      await expect(console_(page).locator(".outcome-error")).toContainText("nothing was upgraded");
      await shot(page, "11-backup-failed");
      expect((await newestJob(page, "kept-space", "upgrade")).error?.kind).toBe("backup-failed");
    } finally {
      await control(page.request, "POST", "/backup/repair");
    }
    const local = (await control(
      page.request,
      "GET",
      "/file?workspace=kept-space&path=local.txt",
    )) as {
      body: string;
    };
    expect(local.body).toContain("fifth: v1");
    expect((await backup(page.request, "kept-space")).commits).toEqual(before.commits);
  });

  test("a reviewed branch is applied, rebuilt and backed up", async () => {
    // A pull request's head: one commit on top of the workspace branch.
    await control(
      page.request,
      "POST",
      "/backup/contribute?from=kept-space&branch=review/kept-space&path=reviewed.txt",
      "reviewed\n",
    );
    const reviewed = (await backup(page.request, "review/kept-space")).commits[0]!;
    const before = await containerFor(page.request, "kept-space");
    await page.goto("/");
    await entry(page, "kept-space").getByRole("button", { name: "Apply branch" }).click();
    await entry(page, "kept-space")
      .getByRole("textbox", { name: "Branch" })
      .fill("review/kept-space");
    await entry(page, "kept-space").getByRole("button", { name: "Apply", exact: true }).click();
    await finished(page, /Apply branch to kept-space/);
    await expect(console_(page).getByRole("log", { name: "Job log" })).toContainText(
      `fetched branch review/kept-space at ${reviewed.hash}`,
    );
    await shot(page, "11a-apply-branch");
    const file = (await control(
      page.request,
      "GET",
      "/file?workspace=kept-space&path=reviewed.txt",
    )) as { body: string };
    expect(file.body).toBe("reviewed\n");
    const branch = await backup(page.request, "kept-space");
    expect(branch.commits.map((c) => c.hash)).toContain(reviewed.hash);
    expect(branch.files["reviewed.txt"]).toBe("reviewed\n");
    // The rebuild replaced the container.
    const after = await containerFor(page.request, "kept-space");
    expect(after?.state).toBe("running");
    expect(after?.id).not.toBe(before?.id);
  });

  test("a held job keeps its workspace busy while another workspace works", async () => {
    await page.goto("/");
    await page.getByRole("link", { name: "New workspace" }).first().click();
    await page.getByRole("radio", { name: /Sample kit, held hook/ }).check();
    await page.getByLabel("Workspace name").fill("held-space");
    await page.getByRole("button", { name: "Begin interview" }).click();
    await page.getByLabel(/^Label/).fill("Held project");
    await page.getByLabel(/^Label/).press("Control+Enter");
    // The hook writes its marker, then waits for the release file.
    await expect
      .poll(async () => {
        const marker = (await control(
          page.request,
          "GET",
          "/file?workspace=held-space&path=hook-marker.txt",
        )) as {
          exists: boolean;
        };
        return marker.exists;
      })
      .toBe(true);
    try {
      expect((await newestJob(page, "held-space", "create")).state).toBe("running");
      expect(await postJob(page, { operation: "start", workspace: "held-space" })).toBe(409);
      expect(
        await postJob(page, {
          operation: "create",
          workspace: "held-space",
          templateId: "sample-kit-v1",
        }),
      ).toBe(409);
      await page.goto("/");
      await expect(entry(page, "held-space")).toHaveClass(/is-busy/);
      await shot(page, "12-held-job");
      // Another workspace stops and starts meanwhile.
      await entry(page, "sample-space").getByRole("button", { name: "Stop" }).click();
      await finished(page, /Stop sample-space/);
      await entry(page, "sample-space").getByRole("button", { name: "Start" }).click();
      await finished(page, /Start sample-space/);
      expect((await newestJob(page, "held-space", "create")).state).toBe("running");
    } finally {
      await control(page.request, "POST", "/release?name=held-space");
    }
    await expect
      .poll(async () => (await newestJob(page, "held-space", "create")).state)
      .toBe("succeeded");
  });

  test("a restart keeps folders and answers and forgets the jobs", async () => {
    await control(page.request, "POST", "/restart");
    // Sessions live in memory: the restart signed everyone out.
    expect((await page.request.get(info.site + "/api/workspaces")).status()).toBe(401);
    await signIn(page, "sample-manager");
    await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible();
    for (const name of ["sample-space", "sample-folder", "kept-space", "held-space"]) {
      await expect(entry(page, name)).toBeVisible();
    }
    await expect(page.getByRole("heading", { name: "Jobs since Menagerie started" })).toHaveCount(
      0,
    );
    const { jobs } = (await (await page.request.get(info.site + "/api/jobs")).json()) as {
      jobs: Snapshot[];
    };
    expect(jobs).toHaveLength(0);
    await shot(page, "13-after-restart");

    // The applied answers are in the workspace: the upgrade asks only the new question.
    await entry(page, "kept-space").getByRole("link", { name: "Upgrade" }).click();
    await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
    await page.getByRole("button", { name: "Begin upgrade" }).click();
    await expect(page.locator("[data-question]")).toHaveCount(1);
    await page.getByRole("radio", { name: "north" }).check();
    await page.getByRole("button", { name: "Send answers" }).click();
    await expect(console_(page).locator(".outcome")).toContainText("Completed.");
    const branch = await backup(page.request, "kept-space");
    expect(branch.files["result.txt"]).toBe("label: Kept project\nsize: small\nregion: north\n");
    // The upgrade's backup kept the applied branch.
    expect(branch.files["reviewed.txt"]).toBe("reviewed\n");
  });
});

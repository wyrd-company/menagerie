import { expect, test, type Page } from "@playwright/test";
import { control, expectNoHorizontalScroll, reset, shot } from "./support.ts";

test.beforeEach(async ({ request }) => reset(request));

async function beginCreate(page: Page, name = "sample-space") {
  await page.goto("/create");
  await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
  await page.getByLabel("Workspace name").fill(name);
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.getByText("Question batch 1")).toBeVisible();
}

const group = (page: Page, name: RegExp) => page.getByRole("group", { name });

test("creates a workspace through a rejected batch, a second batch and the job", async ({
  page,
  request,
}) => {
  await beginCreate(page);
  await expect(page).toHaveURL(/\/create\?job=job-/);
  await expect(
    page.getByRole("region", { name: "Create job" }).locator(".console-head .status"),
  ).toHaveText("Awaiting answers");
  await shot(page, "create-batch-1");

  // Answer every kind of question; two answers only Toha can judge are wrong.
  await page.getByLabel(/^Title/).fill("Sample project");
  await page.getByLabel(/^Notes/).fill("First line\nSecond line");
  await page.getByLabel(/^Remote repository/).fill("https://example.test/sample.git");
  await group(page, /Enable the sample service/)
    .getByRole("radio", { name: "No" })
    .check();
  await group(page, /Verbose logging/)
    .getByRole("radio", { name: "No answer" })
    .check();
  await group(page, /^Size/).getByRole("radio", { name: "small" }).check();
  await group(page, /^Palette/)
    .getByRole("radio", { name: "green" })
    .check();
  await group(page, /^Features/)
    .getByRole("checkbox", { name: "gamma" })
    .check();
  await page.getByRole("button", { name: "Add label" }).click();
  await page.keyboard.type("one");
  await page.keyboard.press("Enter");
  await page.keyboard.type("Bad Label");
  await page
    .getByLabel(/^Settings document/)
    .fill('{"$schema":"https://schemas.example.test/sample-settings/1","count":2}');
  await page.getByRole("button", { name: "Send answers" }).click();

  const summary = page.locator(".error-summary");
  await expect(summary).toBeFocused();
  await expect(summary).toContainText("Label: each item must match ^[a-z0-9-]+$");
  await expect(summary).toContainText('"name" is a required property');
  await expect(page.getByRole("textbox", { name: "Label 2" })).toHaveValue("Bad Label");
  await expect(page.getByLabel(/^Title/)).toHaveValue("Sample project");
  await expect(page.getByText("Question batch 1")).toBeVisible();
  await shot(page, "create-batch-1-rejected");

  await page.getByRole("textbox", { name: "Label 2" }).fill("two");
  await page
    .getByLabel(/^Settings document/)
    .fill('{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample","count":2}');
  await page.getByLabel(/^Settings document/).press("Control+Enter");

  await expect(page.getByText("Question batch 2")).toBeVisible();
  await expect(page.getByRole("list", { name: "Messages from the template" })).toContainText(
    "Sample kit for Sample project.",
  );
  await expect(page.getByLabel(/^Folder name/)).toHaveValue("sample-project");
  await shot(page, "create-batch-2");

  const answers = await (await request.get("/__fixture/requests")).json();
  expect(JSON.parse(answers.answers[0].last)).toEqual({
    template: "https://example.test/templates/sample-kit@v1",
    answers: {
      title: "Sample project",
      notes: "First line\nSecond line",
      remote: "https://example.test/sample.git",
      enabled: false,
      verbose: null,
      size: "small",
      palette: "green",
      features: ["alpha", "gamma"],
      labels: ["one", "two"],
      settings: {
        $schema: "https://schemas.example.test/sample-settings/1",
        name: "sample",
        count: 2,
      },
    },
  });

  // Enter in a text question submits the batch; the untouched default is left to Toha.
  await page.getByLabel(/^Folder name/).press("Enter");
  const job = page.getByRole("region", { name: "Create job" });
  await expect(page.getByText("Question batch 2")).toBeHidden();
  await expect(job.locator(".outcome")).toContainText("Completed.");
  await expect(job.getByRole("list", { name: "Phases" })).toHaveText(
    /template.*interview.*generate.*backup.*initialize-command.*up/s,
  );
  await expect(job.locator(".apply")).toContainText("written");
  await expect(job.locator(".apply")).toContainText(".toha.json");
  await expect(job.getByRole("table", { name: "Files" })).toContainText("README.md");
  await expect(job.locator(".hook.ok")).toContainText("sample-hook exit 0");
  const last = (await (await request.get("/__fixture/requests")).json()).answers[0];
  expect(last.count).toBe(3);
  expect(JSON.parse(last.last).answers).toEqual({});
  await shot(page, "create-completed");

  await page.getByRole("link", { name: "Back to the workspace list" }).click();
  await expect(page.getByRole("heading", { name: "sample-space", exact: true })).toBeVisible();
});

test("returns the staged session byte for byte, and the fixture would notice otherwise", async ({
  page,
  request,
}) => {
  await beginCreate(page);
  const jobId = new URL(page.url()).searchParams.get("job") ?? "";
  // Negative control: a re-serialized session (12345678901234567890 → 12345678901234567000, 1.0 → 1) is refused.
  const changed = await request.post(`/api/jobs/${jobId}/answers`, {
    data: {
      session: JSON.parse(
        '{"target":"/srv/workspaces/sample-space","template":"https://example.test/templates/sample-kit@v1","now":"2026-01-01T00:00:00Z","counter":12345678901234567890,"ratio":1.0,"offset":-0.0,"path":"a\\/b","step":1,"submissions":[]}',
      ),
      submission: { template: "https://example.test/templates/sample-kit@v1", answers: {} },
    },
  });
  expect(changed.status()).toBe(409);
  expect((await changed.json()).error.kind).toBe("session-mismatch");

  // The browser's own request carries the exact text it received.
  const sent = page.waitForRequest((r) => r.url().endsWith("/answers"));
  await page.getByLabel(/^Title/).fill("Sample project");
  await page.getByRole("button", { name: "Send answers" }).click();
  expect((await sent).postData()).toContain(
    '"counter":12345678901234567890,"ratio":1.0,"offset":-0.0,"path":"a\\/b"',
  );
  await expect(page.getByText("Question batch 2")).toBeVisible();
});

test("shows a flow stop as an ended interview that wrote nothing", async ({ page }) => {
  await beginCreate(page, "sample-halted");
  await page.getByLabel(/^Title/).fill("Sample project");
  await group(page, /^Size/).getByRole("radio", { name: "halt" }).check();
  await page.getByRole("button", { name: "Send answers" }).click();
  const notice = page.getByRole("status").filter({ hasText: "ended the interview" });
  await expect(notice).toContainText("stop");
  await expect(notice).toContainText("halt-requested");
  await expect(notice).toContainText("Sample kit for Sample project.");
  const job = page.getByRole("region", { name: "Create job" });
  await expect(job.locator(".console-head .status")).toHaveText("Ended");
  await expect(job.locator(".outcome")).toContainText("The interview ended. Nothing was written.");
  await expect(page.locator("form.interview")).toHaveCount(0);
  await shot(page, "create-ended");
});

test("restores the current batch after a reload", async ({ page }) => {
  await beginCreate(page);
  await page.getByLabel(/^Title/).fill("Sample project");
  await page.getByRole("button", { name: "Send answers" }).click();
  await expect(page.getByText("Question batch 2")).toBeVisible();
  await page.reload();
  await expect(page.getByText("Question batch 2")).toBeVisible();
  await expect(page.getByLabel(/^Folder name/)).toHaveValue("sample-project");
  // The replayed session is the one the server expects next.
  await page.getByLabel(/^Folder name/).press("Enter");
  await expect(page.getByRole("region", { name: "Create job" }).locator(".outcome")).toContainText(
    "Completed.",
  );
});

test("checks the start form and shows the service's refusals", async ({ page }) => {
  await page.goto("/create");
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.getByRole("alert").filter({ hasText: "Choose a template." })).toBeVisible();
  await expect(page.locator(".field.is-invalid")).toContainText("is required");
  await page.getByLabel("Workspace name").fill("sample/space");
  await expect(page.locator(".field.is-invalid")).toContainText(
    "must be one folder name, without slashes",
  );

  await page.getByRole("radio", { name: /Sample kit, version 1/ }).check();
  await page.getByLabel("Workspace name").fill("sample-alpha");
  await page.getByRole("button", { name: "Begin interview" }).click();
  await expect(page.getByRole("alert")).toContainText("workspace-exists");
  await expect(page).toHaveURL(/\/create$/);
});

test("can be driven from the keyboard alone", async ({ page }) => {
  await page.goto("/create");
  await expect(page.getByRole("radio", { name: /Sample kit, version 1/ })).toBeVisible();
  await page.getByRole("radio", { name: /Sample kit, version 1/ }).focus();
  await page.keyboard.press("ArrowDown");
  await expect(page.getByRole("radio", { name: /Sample kit, version 2/ })).toBeChecked();
  await page.keyboard.press("Tab");
  await expect(page.getByLabel("Workspace name")).toBeFocused();
  await page.keyboard.type("sample-keys");
  await page.keyboard.press("Enter");
  await expect(page.getByText("Question batch 1")).toBeVisible();
  // Tab order follows the questions; focus is visible on each.
  await page.locator("form.interview").getByRole("radio").first().focus();
  await page.keyboard.press("ArrowRight");
  await expect(
    group(page, /Enable the sample service/).getByRole("radio", { name: "No" }),
  ).toBeChecked();
  const shadow = await page.evaluate(
    () => getComputedStyle(document.activeElement!.closest(".choice")!).boxShadow,
  );
  expect(shadow).not.toBe("none");
});

test.describe("narrow viewport", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("fits the interview and its job console", async ({ page, request }) => {
    await control(request, "reset");
    await beginCreate(page);
    await expectNoHorizontalScroll(page);
    await shot(page, "create-narrow");
  });
});

test("sends one answers request at a time, however often Ctrl+Enter is pressed", async ({
  page,
}) => {
  await beginCreate(page);
  const held: (() => Promise<void>)[] = [];
  await page.route("**/answers", (route) => {
    held.push(() => route.continue());
  });
  await page.getByLabel(/^Title/).fill("Sample project");
  await page.getByLabel(/^Title/).press("Control+Enter");
  await page.getByLabel(/^Title/).press("Control+Enter");
  await page.getByLabel(/^Title/).press("Enter");
  await expect(page.getByRole("button", { name: "Sending answers…" })).toBeDisabled();
  await expect.poll(() => held.length).toBe(1);
  await page.getByLabel(/^Title/).press("Control+Enter");
  expect(held).toHaveLength(1);
  await held[0]!();
  await page.unroute("**/answers");
  await expect(page.getByText("Question batch 2")).toBeVisible();
});

test("sends a document's large integers exactly as typed", async ({ page }) => {
  await beginCreate(page);
  const text =
    '{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample","count":9007199254740993}';
  await page.getByLabel(/^Title/).fill("Sample project");
  await page.getByLabel(/^Settings document/).fill(text);
  const sent = page.waitForRequest((r) => r.url().endsWith("/answers"));
  await page.getByRole("button", { name: "Send answers" }).click();
  expect((await sent).postData()).toContain(`"settings":${text}`);
});

test("moves focus to a choice question from the error summary", async ({ page }) => {
  await control(page.request, "options", { upgradeDocument: "uncovered" });
  await page.goto("/workspaces/sample-gamma/upgrade");
  await page.getByRole("radio", { name: /Sample kit, version 2/ }).check();
  await page.getByRole("button", { name: "Begin upgrade" }).click();
  await page.getByRole("button", { name: "Send answers" }).click();
  await page.locator(".error-summary").getByRole("link", { name: "Region" }).click();
  await expect(page.getByRole("radio", { name: "north" })).toBeFocused();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("radio", { name: "south" })).toBeChecked();
});

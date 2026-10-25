// Helpers the integrated and acceptance specs share: the fixture's
// description, its control server, Pocket ID sign-in and the list's parts.
import { readFileSync } from "node:fs";
import { expect, type APIRequestContext, type Browser, type Page } from "@playwright/test";

export const info = JSON.parse(readFileSync(process.env.MENAGERIE_BROWSER_INFO!, "utf8")) as {
  site: string;
  pocket: string;
  control: string;
  root: string;
};

// The acceptance run keeps its evidence in its scratch folder.
export const shot = (page: Page, name: string) =>
  page.screenshot({
    path: `${process.env.MENAGERIE_EVIDENCE ?? "evidence"}/integrated/${name}.png`,
    fullPage: true,
  });

export async function control(
  request: APIRequestContext,
  method: "GET" | "POST" | "PUT",
  path: string,
  body?: string,
) {
  const response = await request.fetch(info.control + path, { method, data: body });
  expect(response.ok(), `${method} ${path}`).toBeTruthy();
  return response.json();
}

/** Signs a Pocket ID user in through its one-time link, then opens Menagerie and consents. */
export async function signIn(page: Page, user: string, target = "/") {
  const { url } = await control(page.request, "GET", `/signin-link?user=${user}`);
  await page.goto(url);
  await page.waitForURL((u) => !u.pathname.startsWith("/lc/"));
  await page.goto(info.site + target);
  if (new URL(page.url()).pathname === "/interaction") {
    await page.getByRole("button", { name: "Sign in" }).click();
  }
}

export const entry = (page: Page, name: string) =>
  page
    .getByRole("list", { name: "Workspaces" })
    .getByRole("listitem")
    .filter({
      has: page.getByRole("heading", { name, exact: true }),
    });
export const console_ = (page: Page) => page.locator("section.console");

/** Waits for the console of the job just started, then for its outcome. */
export async function finished(page: Page, heading: RegExp, outcome = "Completed.") {
  await expect(console_(page).getByRole("heading", { level: 2 })).toHaveText(heading);
  await expect(console_(page).locator(".outcome")).toContainText(outcome);
}

export async function containerFor(request: APIRequestContext, name: string) {
  const list = (await control(request, "GET", "/containers")) as {
    id: string;
    state: string;
    folder: string;
  }[];
  return list.find((c) => c.folder === `${info.root}/${name}`);
}

export async function backup(request: APIRequestContext, branch: string) {
  return (await control(request, "GET", `/backup?branch=${branch}`)) as {
    exists: boolean;
    commits: { hash: string; message: string; parents: number }[];
    files: Record<string, string>;
  };
}

/** A page of its own; the acceptance run records its video with the evidence. */
export const ownPage = (browser: Browser) =>
  browser.newPage(
    process.env.MENAGERIE_EVIDENCE
      ? { recordVideo: { dir: `${process.env.MENAGERIE_EVIDENCE}/videos` } }
      : {},
  );

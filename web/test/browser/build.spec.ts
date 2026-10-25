import { existsSync, readdirSync, readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

// web/embed.go embeds dist/; the build must leave its placeholder in place.
test("the build keeps the embed placeholder beside the UI", () => {
  expect(existsSync("dist/.gitkeep"), "dist/.gitkeep").toBe(true);
  expect(readFileSync("dist/.gitkeep", "utf8")).toBe("");
  expect(readFileSync("dist/index.html", "utf8")).toContain('<div id="root"></div>');
});

// The workspace service mounts only index.html and assets/ from the build.
test("the build holds nothing the workspace service does not serve", () => {
  const top = readdirSync("dist").sort();
  expect(top).toEqual([".gitkeep", "assets", "index.html"]);
  const html = readFileSync("dist/index.html", "utf8");
  const refs = [...html.matchAll(/(?:src|href)="([^"]+)"/g)].map((m) => m[1]!);
  for (const ref of refs) {
    expect(ref.startsWith("/assets/") || ref.startsWith("data:"), ref).toBe(true);
  }
});

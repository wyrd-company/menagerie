import { existsSync, readFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

// web/embed.go embeds dist/; the build must leave its placeholder in place.
test("the build keeps the embed placeholder beside the UI", () => {
  expect(existsSync("dist/.gitkeep"), "dist/.gitkeep").toBe(true);
  expect(readFileSync("dist/.gitkeep", "utf8")).toBe("");
  expect(readFileSync("dist/index.html", "utf8")).toContain('<div id="root"></div>');
});

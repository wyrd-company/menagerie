import { describe, expect, it } from "vitest";
import { nameProblems } from "./CreateWorkspace.tsx";

describe("nameProblems", () => {
  it("accepts a plain folder name", () => {
    expect(nameProblems("sample-space")).toEqual([]);
  });

  it("accepts a dot after the first character", () => {
    expect(nameProblems("sample.space")).toEqual([]);
  });

  it.each([".", "..", ".hidden"])("refuses %s, which starts with a dot", (name) => {
    expect(nameProblems(name)).toEqual(["must not start with a dot"]);
  });
});

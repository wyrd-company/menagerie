// Versions compare by their numbers, each read whole.
import { describe, expect, it } from "vitest";
import { newer } from "./versions.ts";

describe("newer", () => {
  it("orders versions by major, minor and patch, a leading v or not", () => {
    expect(newer("1.10.0", "1.9.0")).toBe(true);
    expect(newer("v2.0.0", "1.99.99")).toBe(true);
    expect(newer("1.0.1", "v1.0.1")).toBe(false);
    expect(newer("1.0.0", "1.0.1")).toBe(false);
  });

  it("tells apart numbers above 2^53 that a Number rounds together", () => {
    expect(Number("9007199254740993")).toBe(Number("9007199254740992"));
    expect(newer("9007199254740993.0.0", "9007199254740992.0.0")).toBe(true);
    expect(newer("1.0.18446744073709551615", "1.0.18446744073709551614")).toBe(true);
    expect(newer("9007199254740992.0.0", "9007199254740993.0.0")).toBe(false);
  });
});

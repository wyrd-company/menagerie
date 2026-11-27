// Versions compare by their numbers, each read whole.
import { describe, expect, it } from "vitest";
import type { Template } from "./types.ts";
import { checkLine, newer } from "./versions.ts";

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

describe("checkLine", () => {
  const at = "2026-01-01T00:00:00Z";
  const now = Date.parse(at);
  const failed = (failure: string, credential: Template["credential"]): Template => ({
    id: "kit",
    name: "Kit",
    address: "https://git.example.invalid/example-org/kit.git",
    currentVersion: "1.0.0",
    runnerImage: null,
    credential,
    workspaces: [],
    newestVersion: "1.0.0",
    versions: [],
    currentMoved: false,
    lastCheck: { at, failure },
  });

  it("names the credential the Git host refused", () => {
    expect(checkLine(failed("template-resolve-failed", { githubApp: "sample-org" }), now)).toBe(
      "Check failed just now: the Git host refused or failed the listing (fetched with GitHub App sample-org). Previously found versions are kept.",
    );
    expect(
      checkLine(
        failed("template-not-found", { sshKey: "ssh-ed25519 AAAA", fingerprint: "SHA256:sample" }),
        now,
      ),
    ).toBe(
      "Check failed just now: the Git host has no such repository (fetched with SSH key SHA256:sample). Previously found versions are kept.",
    );
  });

  it("names no credential for a failure the Git host did not answer, or for none", () => {
    expect(
      checkLine(failed("template-service-unavailable", { githubApp: "sample-org" }), now),
    ).toBe(
      "Check failed just now: template service is unavailable. Previously found versions are kept.",
    );
    expect(checkLine(failed("template-resolve-failed", null), now)).toBe(
      "Check failed just now: the Git host refused or failed the listing. Previously found versions are kept.",
    );
  });
});

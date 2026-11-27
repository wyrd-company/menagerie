import { expect, it } from "vitest";

import { describeCredential, effectiveChoice } from "./CredentialPicker.tsx";

it("says when a repository's App or key is no longer there", () => {
  const credentials = {
    githubApps: [{ name: "a", server: "https://git.example.test" }],
    sshKeys: [],
    sshAgent: null,
  };
  const url = "https://git.example.test/o/r";
  expect(describeCredential({ githubApp: "a" }, url, credentials)).toBe("GitHub App a");
  expect(describeCredential({ githubApp: "gone" }, url, credentials)).toBe(
    "GitHub App gone, which is no longer configured",
  );
  expect(
    describeCredential(
      { sshKey: "ssh-ed25519 AAAA", fingerprint: "SHA256:gone" },
      "git@h:o/r",
      credentials,
    ),
  ).toBe("SSH key SHA256:gone, which the SSH agent no longer offers");
});

it("matches a saved App's name in any case, as the service does", () => {
  const credentials = {
    githubApps: [{ name: "team-a", server: "https://git.example.test" }],
    sshKeys: [],
    sshAgent: null,
  };
  const url = "https://git.example.test/o/r";
  expect(describeCredential({ githubApp: "Team-A" }, url, credentials)).toBe("GitHub App team-a");
  expect(effectiveChoice(url, credentials, { githubApp: "Team-A" })).toEqual({
    githubApp: "team-a",
  });
});

it("says when a saved App no longer serves the repository, and keeps it chosen", () => {
  const credentials = {
    githubApps: [
      { name: "team-a", server: "https://elsewhere.example.test" },
      { name: "team-b", server: "https://git.example.test" },
    ],
    sshKeys: [],
    sshAgent: null,
  };
  const url = "https://git.example.test/o/r";
  expect(describeCredential({ githubApp: "team-a" }, url, credentials)).toBe(
    "GitHub App team-a, which no longer serves this repository's server",
  );
  // The only App that serves the URL is no replacement for the saved one.
  expect(effectiveChoice(url, credentials, { githubApp: "team-a" })).toBeNull();
  expect(effectiveChoice(url, credentials, null)).toEqual({ githubApp: "team-b" });
});

it("does not swap a saved key the agent no longer offers for its only key", () => {
  const credentials = {
    githubApps: [],
    sshKeys: [{ name: "other", publicKey: "ssh-ed25519 BBBB", fingerprint: "SHA256:other" }],
    sshAgent: null,
  };
  expect(effectiveChoice("git@h:o/r", credentials, { sshKey: "ssh-ed25519 AAAA" })).toBeNull();
  expect(effectiveChoice("git@h:o/r", credentials, null)).toEqual({ sshKey: "ssh-ed25519 BBBB" });
});

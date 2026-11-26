import { expect, it } from "vitest";

import { describeCredential } from "./CredentialPicker.tsx";

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

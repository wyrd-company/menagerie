import { expect, it } from "vitest";

import { servingApps, urlKind } from "./credentialUrls.ts";

// The cases mirror the backend's: go-git's endpoint parsing decides the
// kind, and githubapp's Minter.Repository decides the Apps serving a URL.
it("reads a URL's kind as the service does", () => {
  expect(urlKind("ssh://git@git.example.test/o/r.git")).toBe("ssh");
  expect(urlKind("SSH://git.example.test/o/r.git")).toBe("ssh");
  expect(urlKind("git@git.example.test:o/r.git")).toBe("ssh");
  expect(urlKind("git.example.test:o/r.git")).toBe("ssh");
  expect(urlKind("git.example.test:2222:o/r.git")).toBe("ssh");
  expect(urlKind("https://git.example.test/o/r.git")).toBe("https");
  expect(urlKind("HTTPS://git.example.test/o/r.git")).toBe("https");
  expect(urlKind("http://git.example.test/o/r.git")).toBeNull();
  expect(urlKind("file:///srv/r.git")).toBeNull();
  expect(urlKind("./local:path")).toBeNull();
  expect(urlKind("/srv/with:colon")).toBeNull();
  expect(urlKind("")).toBeNull();
});

it("finds the Apps that serve an https URL as the service does", () => {
  const apps = [
    { name: "a", server: "https://git.example.test" },
    { name: "b", server: "https://GIT.example.test:443/" },
    { name: "c", server: "https://elsewhere.example.test" },
  ];
  const names = (url: string) => servingApps(url, apps).map((a) => a.name);
  expect(names("https://git.example.test/o/r.git")).toEqual(["a", "b"]);
  expect(names("https://git.example.test:443/o/r")).toEqual(["a", "b"]);
  expect(names("https://Git.Example.Test/o/r")).toEqual(["a", "b"]);
  expect(names("https://git.example.test:8443/o/r")).toEqual([]);
  expect(names("https://user@git.example.test/o/r")).toEqual([]);
  expect(names("https://git.example.test/o/r?x=1")).toEqual([]);
  expect(names("https://git.example.test/o/r#f")).toEqual([]);
  expect(names("https://git.example.test/o")).toEqual([]);
  expect(names("https://git.example.test/o/r/extra")).toEqual([]);
  expect(names("https://git.example.test/-o/r")).toEqual([]);
  expect(names("ssh://git.example.test/o/r")).toEqual([]);
});

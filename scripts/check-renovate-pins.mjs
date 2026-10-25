// Check that .github/renovate.json's regex managers capture every version
// this repository pins outside a package manifest: each `# renovate:`
// comment, and every test image pinned by tag and digest. A pattern that
// matches nothing is not an error to Renovate, so this check is the one
// that notices.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

const config = JSON.parse(readFileSync(".github/renovate.json", "utf8"));
const files = execFileSync("git", ["ls-files", "-z"], { encoding: "utf8" }).split("\0").filter(Boolean);
const failures = [];
const found = [];

// Renovate's (?<name>) groups are JavaScript's too.
for (const manager of config.customManagers) {
  const patterns = manager.managerFilePatterns.map((p) => new RegExp(p.slice(1, -1)));
  for (const file of files.filter((f) => patterns.some((p) => p.test(f)))) {
    const text = readFileSync(file, "utf8");
    for (const source of manager.matchStrings) {
      for (const m of text.matchAll(new RegExp(source, "g"))) {
        found.push({ file, index: m.index, end: m.index + m[0].length, ...m.groups });
      }
    }
  }
}

// Every renovate comment opens a match.
for (const file of files) {
  const text = readFileSync(file, "utf8");
  for (const m of text.matchAll(/# renovate: /g)) {
    if (file === "scripts/check-renovate-pins.mjs" || file === ".github/renovate.json") continue;
    if (!found.some((f) => f.file === file && f.index === m.index)) {
      const line = text.slice(0, m.index).split("\n").length;
      failures.push(`${file}:${line}: no regex manager captures this renovate comment`);
    }
  }
}

// Every tag-and-digest pin in the files the image manager reads is captured.
const digestManager = config.customManagers.find((m) => m.datasourceTemplate === "docker");
const digestFiles = digestManager.managerFilePatterns.map((p) => new RegExp(p.slice(1, -1)));
for (const file of files.filter((f) => digestFiles.some((p) => p.test(f)))) {
  const text = readFileSync(file, "utf8");
  for (const m of text.matchAll(/@sha256:[a-f0-9]{64}/g)) {
    if (!found.some((f) => f.file === file && f.index < m.index && f.end >= m.index + m[0].length)) {
      const line = text.slice(0, m.index).split("\n").length;
      failures.push(`${file}:${line}: a digest pin no regex manager captures`);
    }
  }
}

const required = [
  ["ghcr.io/pocket-id/pocket-id", "backend/cmd/menagerie/pocketid_test.go"],
  ["docker", "scripts/isolated-docker.sh"],
  ["ghcr.io/wyrd-company/devcontainers/base", "scripts/isolated-docker.sh"],
  ["just-containers/s6-overlay", "Dockerfile"],
  ["openbao/openbao", "taskfile.yml"],
];
for (const [depName, file] of required) {
  if (!found.some((f) => f.depName === depName && f.file === file)) {
    failures.push(`${file}: ${depName} is not captured`);
  }
}

for (const f of found) {
  console.log(`${f.file}: ${f.depName} ${f.currentValue}${f.currentDigest ? "@" + f.currentDigest.slice(0, 19) : ""}`);
}
if (failures.length > 0) {
  console.error(failures.join("\n"));
  process.exit(1);
}

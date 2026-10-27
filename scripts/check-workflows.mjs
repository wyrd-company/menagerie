// Check every workflow job's step order and tool bootstrap, for a clean
// GitHub-hosted runner where the workspace starts empty and only the
// runner image's own tools exist:
//   1. a step that runs a repository task comes after the job's first
//      actions/checkout and go-task/setup-task;
//   2. every tool a task needs is installed by an earlier step of the same
//      job, per the table below.
// The table is an agreement with the taskfile: a task the table does not
// know fails the check until its prerequisites are recorded here.
// Usage: node scripts/check-workflows.mjs [workflow.yml...]; the default is
// every file in .github/workflows. yq reads the YAML.
import { execFileSync } from "node:child_process";
import { readdirSync } from "node:fs";

const go = "actions/setup-go";
const rust = "dtolnay/rust-toolchain";
const pnpm = "pnpm/action-setup";
const node = "actions/setup-node";
const task = (name) => `task:${name}`;
const rustWith = (component) => `${rust}[${component}]`;

// What each task needs on a clean runner besides git, gh, jq, yq, curl and
// Docker, which the runner image has.
const needs = {
  lint: [go, rustWith("clippy"), rustWith("rustfmt"), pnpm, node, task("tools:install:linters")],
  "lint:commit-msg": [],
  // The hook test lints Markdown with rumdl and formats Rust with rustfmt.
  test: [go, rustWith("rustfmt"), pnpm, node, task("tools:install:browser"), task("tools:install:intentional"),
    task("tools:install:lefthook"), task("tools:install:linters")],
  build: [go, rust, pnpm, node],
  "release:rehearse": [task("tools:install:intentional")],
  "release:tag": [],
  "release:image": ["docker/setup-qemu-action", "docker/setup-buildx-action", "docker/login-action"],
  "settings:credentials": [],
  "settings:apply": [],
  "audit:summary": [],
  "tools:install:linters": [go, rust],
  "tools:install:intentional": [rust],
  "tools:install:lefthook": [go],
  "tools:install:browser": [pnpm, node],
  "tools:install:openbao": [],
  "test:scripts:docker": [],
  "test:docker": [go],
  "test:docker:proxy": [go],
  "test:openbao": [go, task("tools:install:openbao")],
  "test:pocketid": [go],
  "test:templateservice": [go, rust],
  "test:templateservice:docker": [go, rust],
  "test:integrated": [go, rust, pnpm, node, task("tools:install:browser")],
  "test:image": [go],
  "test:acceptance": [go, pnpm, node, task("tools:install:browser")],
  "test:image:cross": [],
};

const files = process.argv.length > 2
  ? process.argv.slice(2)
  : readdirSync(".github/workflows").filter((f) => f.endsWith(".yml")).map((f) => `.github/workflows/${f}`);
const workflows = files.map((file) => ({ file, ...JSON.parse(execFileSync("yq", ["-o=json", file], { encoding: "utf8" })) }));
const failures = [];
let checked = 0;
for (const wf of workflows) {
  for (const [job, def] of Object.entries(wf.jobs ?? {})) {
    const where = `${wf.file} ${job}`;
    const steps = (def.steps ?? []).map((s) => {
      const uses = s.uses ? s.uses.split("@")[0] : null;
      const components = String(s.with?.components ?? "").split(/[\s,]+/).filter(Boolean);
      const tasks = [...String(s.run ?? "").matchAll(/(?:^|[\s;&|(])task ([a-z][a-z0-9:-]*)/g)].map((m) => m[1]);
      return { uses, components, tasks };
    });
    const firstOf = (name) => steps.findIndex((s) => s.uses === name);
    steps.forEach((step, i) => {
      if (step.tasks.length === 0) return;
      for (const [name, label] of [["actions/checkout", "checks out"], ["go-task/setup-task", "installs Task"]]) {
        const at = firstOf(name);
        if (at < 0 || at > i) failures.push(`${where} step ${i + 1} runs task ${step.tasks[0]} before a step ${label}`);
      }
      for (const t of step.tasks) {
        checked++;
        if (!(t in needs)) {
          failures.push(`${where} runs task ${t}, whose prerequisites check-workflows.mjs does not record`);
          continue;
        }
        for (const need of needs[t]) {
          const earlier = steps.slice(0, i);
          const met = need.startsWith("task:")
            ? earlier.some((s) => s.tasks.includes(need.slice(5)))
            : need.includes("[")
              ? earlier.some((s) => s.uses === rust && s.components.includes(need.slice(rust.length + 1, -1)))
              : earlier.some((s) => s.uses === need);
          if (!met) failures.push(`${where} runs task ${t} without an earlier ${need}`);
        }
      }
    });
  }
}
if (checked === 0) failures.push("no task steps found: is the input the workflows?");
if (failures.length > 0) {
  console.error(failures.join("\n"));
  process.exit(1);
}
console.log(`checked ${checked} task steps in ${workflows.length} workflows`);

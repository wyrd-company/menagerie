// Scripted jobs. Container jobs walk the runtime's phases; create and upgrade
// replay the Toha-produced documents. Jobs on "sample-slow" wait at their
// slowest step until a test releases them.
import { document, finish, interview, log, setState } from "./events.ts";
import { containerId, type FixtureJob, type World } from "./state.ts";

const tick = () => new Promise<void>((resolve) => setImmediate(resolve));
const hold = (job: FixtureJob) =>
  job.workspace === "sample-slow"
    ? new Promise<void>((resolve) => {
        job.release = resolve;
      })
    : tick();

const workspace = (world: World, name: string) => world.workspaces.find((w) => w.name === name);

async function runContainerJob(world: World, job: FixtureJob) {
  const ws = workspace(world, job.workspace);
  if (!ws) return;
  if (job.operation === "stop") {
    setState(job, "running", "stop");
    log(job, `stopping container ${ws.containerId?.slice(0, 12)}`);
    await hold(job);
    ws.state = "stopped";
    log(job, "container stopped");
    finish(job, "succeeded");
    return;
  }
  setState(job, "running", "read-definition");
  setState(job, "running", "initialize-command");
  log(job, "$ ./scripts/initialize.sh");
  if (ws.name === "sample-broken") {
    log(job, "initialize: missing sample marker");
    finish(job, "failed", {
      kind: "initialize-command-failed",
      message: "initializeCommand exited with status 3",
    });
    return;
  }
  log(job, "initialize: ok");
  if (job.operation === "rebuild") {
    setState(job, "running", "build");
    for (let n = 1; n <= 30; n++) log(job, `#${n} building layer ${n} of 30`);
    await hold(job);
    setState(job, "running", "remove-container");
    log(job, "removed previous container");
  }
  setState(job, "running", "up");
  if (job.operation === "start") await hold(job);
  ws.state = "running";
  ws.containerId ??= containerId(ws.name);
  log(job, `container ${ws.containerId.slice(0, 12)} running`);
  finish(job, "succeeded");
}

async function runCreate(job: FixtureJob) {
  setState(job, "running", "template");
  log(job, "resolving the approved template");
  await tick();
  setState(job, "waiting-for-answers", "interview");
  interview(job, "questions", document("create-batch-1.json"));
}

async function runUpgrade(world: World, job: FixtureJob) {
  setState(job, "running", "backup");
  log(job, "committed tracked changes");
  log(job, "pushed backup branch");
  await tick();
  setState(job, "waiting-for-answers", "interview");
  const name =
    world.options.upgradeDocument === "raw"
      ? "upgrade-batch-1.json"
      : "upgrade-batch-uncovered.json";
  interview(job, "questions", document(name));
}

async function complete(world: World, job: FixtureJob) {
  setState(job, "running", job.operation === "create" ? "generate" : "apply");
  log(job, "template generation started");
  await tick();
  const files = [{ path: "README.md", action: job.operation === "create" ? "create" : "update" }];
  log(job, "template generation finished: 1 file");
  if (job.operation === "upgrade" && job.workspace === "sample-beta") {
    finish(
      job,
      "failed",
      { kind: "merge-conflict", message: "The upgrade left 1 path with conflicts" },
      {
        outcome: "conflicted",
        files,
        recordPath: ".toha.json",
        messages: [],
        hooks: [],
        conflicts: ["README.md"],
      },
    );
    return;
  }
  setState(job, "running", "backup");
  log(job, `pushed refs/heads/${job.workspace}`);
  setState(job, "running", "initialize-command");
  log(job, "initialize: ok");
  setState(job, "running", job.operation === "create" ? "up" : "rebuild");
  let ws = workspace(world, job.workspace);
  if (!ws) {
    ws = { name: job.workspace, state: "running", containerId: null };
    world.workspaces.push(ws);
    world.workspaces.sort((a, b) => a.name.localeCompare(b.name));
  }
  ws.state = "running";
  ws.containerId = containerId(job.workspace);
  log(job, `container ${ws.containerId.slice(0, 12)} running`);
  finish(job, "succeeded", null, {
    outcome: "written",
    files,
    recordPath: ".toha.json",
    messages: ["Sample kit for Sample project."],
    hooks: [{ id: "sample-hook", exitCode: 0, success: true }],
    conflicts: [],
  });
}

/** Replays the create interview: batch 1 (or its rejection, or a flow stop), then batch 2. */
export function answer(
  world: World,
  job: FixtureJob,
  submission: { template: string; answers: Record<string, unknown> },
): string {
  job.answerCount += 1;
  job.lastSubmission = JSON.stringify(submission);
  const { answers } = submission;
  if (job.operation === "create" && job.stage === "first") {
    // The producer rejected a label off its pattern and a settings document without "name".
    const labels = Array.isArray(answers.labels) ? answers.labels : [];
    const settings = answers.settings as Record<string, unknown> | undefined;
    if (
      labels.some((l) => !/^[a-z0-9-]+$/.test(String(l))) ||
      (settings && !("name" in settings))
    ) {
      return interview(job, "questions", document("create-batch-1-rejected.json"));
    }
    if (answers.size === "halt") {
      const reply = interview(job, "ended", document("create-ended.json"));
      finish(job, "ended");
      return reply;
    }
    job.stage = "second";
    return interview(job, "questions", document("create-batch-2.json"));
  }
  setState(job, "running", job.operation === "create" ? "generate" : "apply");
  const reply = interview(job, "completed", null);
  void complete(world, job);
  return reply;
}

/** Runs a job's script after its start request has been answered. */
export function run(world: World, job: FixtureJob) {
  if (job.operation === "create") void runCreate(job);
  else if (job.operation === "upgrade") void runUpgrade(world, job);
  else void runContainerJob(world, job);
}

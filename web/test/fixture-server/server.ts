// Test-only fixture of the workspace service's browser contract
// (web/docs/browser-api.md). It serves the built UI and answers /api with
// in-memory workspaces and jobs. Interviews replay the Toha documents in
// test/fixtures/toha/documents, which a real Toha build produced.
//
// It is not part of Menagerie: no OIDC, no Docker, no Toha. Routes under
// /__fixture let browser tests steer it.
import { randomUUID } from "node:crypto";
import { createReadStream, existsSync, readFileSync, statSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { extname, join, normalize } from "node:path";

const documentsDir = new URL("../fixtures/toha/documents/", import.meta.url);
function document(name: string): string {
  return readFileSync(new URL(name, documentsDir), "utf8").trim();
}

interface FixtureWorkspace {
  name: string;
  state: "running" | "stopped" | "no-container";
  containerId: string | null;
}

interface StoredEvent {
  id: number;
  type: string;
  data: string;
}

interface FixtureJob {
  jobId: string;
  workspace: string;
  operation: string;
  templateId?: string;
  state: string;
  phase: string;
  createdAt: string;
  finishedAt: string | null;
  error: { kind: string; message: string } | null;
  events: StoredEvent[];
  listeners: Set<ServerResponse>;
  /** The exact session text the job last sent; answers must carry it back unchanged. */
  sentSession: string | null;
  step: number;
  release: (() => void) | null;
  answerCount: number;
  lastSubmission?: string;
  stage: "first" | "second";
}

export interface FixtureOptions {
  /** "raw": the producer's whole upgrade batch; "uncovered": the narrowed synthetic one. */
  upgradeDocument: "raw" | "uncovered";
}

const containerId = (seed: string) =>
  Array.from(
    { length: 64 },
    (_, i) => "0123456789abcdef"[(seed.charCodeAt(i % seed.length) + i) % 16],
  ).join("");

function initialWorkspaces(): FixtureWorkspace[] {
  return [
    { name: "sample-alpha", state: "running", containerId: containerId("alpha") },
    { name: "sample-beta", state: "stopped", containerId: containerId("beta") },
    { name: "sample-broken", state: "stopped", containerId: containerId("broken") },
    { name: "sample-gamma", state: "no-container", containerId: null },
    { name: "sample-slow", state: "running", containerId: containerId("slow") },
  ];
}

const templates = [
  { id: "sample-kit-v1", name: "Sample kit, version 1" },
  { id: "sample-kit-v2", name: "Sample kit, version 2" },
];

/**
 * A staged session as opaque JSON text. It holds values that JSON.parse would
 * change on the way back (a 20-digit integer, 1.0, -0.0, an escaped solidus),
 * so only a byte-for-byte return matches.
 */
function sessionText(job: FixtureJob): string {
  return `{"target":"/srv/workspaces/${job.workspace}","template":"https://example.test/templates/sample-kit@v1","now":"2026-01-01T00:00:00Z","counter":12345678901234567890,"ratio":1.0,"offset":-0.0,"path":"a\\/b","step":${job.step},"submissions":[]}`;
}

export function createFixture(options: { distDir: string }) {
  let workspaces = initialWorkspaces();
  let jobs = new Map<string, FixtureJob>();
  let signedIn = true;
  let dockerAvailable = true;
  let fixtureOptions: FixtureOptions = { upgradeDocument: "raw" };
  let sequence = 0;

  const reset = () => {
    for (const job of jobs.values()) for (const res of job.listeners) res.end();
    workspaces = initialWorkspaces();
    jobs = new Map();
    signedIn = true;
    dockerAvailable = true;
    fixtureOptions = { upgradeDocument: "raw" };
    sequence = 0;
  };

  const summary = (job: FixtureJob) => ({
    jobId: job.jobId,
    workspace: job.workspace,
    operation: job.operation,
    state: job.state,
    phase: job.phase,
    createdAt: job.createdAt,
    finishedAt: job.finishedAt,
    lastEventId: job.events.length,
    error: job.error,
  });

  const send = (res: ServerResponse, event: StoredEvent) => {
    res.write(`id: ${event.id}\nevent: ${event.type}\n`);
    for (const line of event.data.split("\n")) res.write(`data: ${line}\n`);
    res.write("\n");
  };

  const emit = (job: FixtureJob, type: string, data: string) => {
    const event = { id: job.events.length + 1, type, data };
    job.events.push(event);
    for (const res of job.listeners) {
      send(res, event);
      if (type === "result") res.end();
    }
    if (type === "result") job.listeners.clear();
  };

  const setState = (job: FixtureJob, state: string, phase: string) => {
    job.state = state;
    job.phase = phase;
    emit(job, "state", JSON.stringify({ state, phase }));
  };

  const log = (job: FixtureJob, text: string) => emit(job, "log", JSON.stringify({ text }));

  const finish = (
    job: FixtureJob,
    state: "succeeded" | "failed" | "ended",
    error: FixtureJob["error"] = null,
    apply?: unknown,
  ) => {
    job.state = state;
    job.error = error;
    job.finishedAt = new Date().toISOString();
    const result: Record<string, unknown> = { state, error };
    if (apply !== undefined) result.apply = apply;
    emit(job, "result", JSON.stringify(result));
  };

  const interview = (job: FixtureJob, state: string, doc: string | null) => {
    job.step += 1;
    const session = sessionText(job);
    job.sentSession = session;
    // The reply is assembled as text so the session is exactly what was sent.
    const reply = `{"session":${session},"state":${JSON.stringify(state)},"document":${doc ?? "null"},"warnings":[]}`;
    emit(job, "interview", reply);
    return reply;
  };

  const tick = () => new Promise<void>((resolve) => setImmediate(resolve));
  const hold = (job: FixtureJob) =>
    job.workspace === "sample-slow"
      ? new Promise<void>((resolve) => {
          job.release = resolve;
        })
      : tick();

  const workspace = (name: string) => workspaces.find((w) => w.name === name);

  async function runContainerJob(job: FixtureJob) {
    const ws = workspace(job.workspace);
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

  async function runUpgrade(job: FixtureJob) {
    setState(job, "running", "backup");
    log(job, "committed tracked changes");
    log(job, "pushed backup branch");
    await tick();
    setState(job, "waiting-for-answers", "interview");
    const name =
      fixtureOptions.upgradeDocument === "raw"
        ? "upgrade-batch-1.json"
        : "upgrade-batch-uncovered.json";
    interview(job, "questions", document(name));
  }

  async function complete(job: FixtureJob) {
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
    let ws = workspace(job.workspace);
    if (!ws) {
      ws = { name: job.workspace, state: "running", containerId: null };
      workspaces.push(ws);
      workspaces.sort((a, b) => a.name.localeCompare(b.name));
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
  function answer(
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
    void complete(job);
    return reply;
  }

  async function readBody(req: IncomingMessage): Promise<string> {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk as Buffer);
    return Buffer.concat(chunks).toString("utf8");
  }

  const json = (
    res: ServerResponse,
    status: number,
    value: unknown,
    headers: Record<string, string> = {},
  ) => {
    res.writeHead(status, { "Content-Type": "application/json", ...headers });
    res.end(JSON.stringify(value));
  };
  const fail = (
    res: ServerResponse,
    status: number,
    kind: string,
    message: string,
    extra: object = {},
  ) => json(res, status, { error: { kind, message, ...extra } });

  const mime: Record<string, string> = {
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript",
    ".css": "text/css",
    ".svg": "image/svg+xml",
    ".woff2": "font/woff2",
    ".woff": "font/woff",
    ".map": "application/json",
  };

  function serveStatic(res: ServerResponse, pathname: string) {
    const root = options.distDir;
    const candidate = normalize(join(root, pathname));
    const file =
      candidate.startsWith(root) && existsSync(candidate) && statSync(candidate).isFile()
        ? candidate
        : join(root, "index.html");
    if (!existsSync(file)) {
      res.writeHead(500, { "Content-Type": "text/plain" });
      res.end("The UI is not built: run pnpm build.");
      return;
    }
    res.writeHead(200, { "Content-Type": mime[extname(file)] ?? "application/octet-stream" });
    createReadStream(file).pipe(res);
  }

  async function handle(req: IncomingMessage, res: ServerResponse) {
    const url = new URL(req.url ?? "/", "http://fixture");
    const { pathname } = url;
    const method = req.method ?? "GET";

    // ---- Test controls ----
    if (pathname.startsWith("/__fixture/")) {
      const body = method === "POST" ? await readBody(req) : "";
      const input = body ? (JSON.parse(body) as Record<string, unknown>) : {};
      if (pathname === "/__fixture/health") return json(res, 200, { ok: true });
      if (pathname === "/__fixture/reset") reset();
      else if (pathname === "/__fixture/auth") signedIn = input.signedIn === true;
      else if (pathname === "/__fixture/docker") dockerAvailable = input.available === true;
      else if (pathname === "/__fixture/options")
        fixtureOptions = { ...fixtureOptions, ...(input as Partial<FixtureOptions>) };
      else if (pathname === "/__fixture/restart") jobs = new Map();
      else if (pathname.startsWith("/__fixture/release/")) {
        const job = jobs.get(decodeURIComponent(pathname.slice("/__fixture/release/".length)));
        job?.release?.();
        if (job) job.release = null;
      } else if (pathname === "/__fixture/requests") {
        return json(res, 200, {
          answers: [...jobs.values()].map((j) => ({
            jobId: j.jobId,
            count: j.answerCount,
            last: j.lastSubmission ?? null,
          })),
        });
      } else return fail(res, 404, "unknown-control", pathname);
      return json(res, 200, { ok: true });
    }

    // ---- OIDC stand-ins ----
    if (pathname === "/auth/login") {
      const back = url.searchParams.get("return") ?? "/";
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(
        `<!doctype html><title>Fixture sign-in</title><h1>Fixture sign-in</h1><p id="return">${back.replace(/[<&"]/g, "")}</p><form method="post" action="/auth/fixture-sign-in?return=${encodeURIComponent(back)}"><button>Sign in</button></form>`,
      );
      return;
    }
    if (pathname === "/auth/fixture-sign-in" && method === "POST") {
      signedIn = true;
      const back = url.searchParams.get("return") ?? "/";
      res.writeHead(303, { Location: back.startsWith("/") && !back.startsWith("//") ? back : "/" });
      res.end();
      return;
    }
    if (pathname === "/auth/logout" && method === "POST") {
      signedIn = false;
      res.writeHead(303, { Location: "/auth/login?return=%2F" });
      res.end();
      return;
    }

    if (!pathname.startsWith("/api/")) return serveStatic(res, pathname);
    if (!signedIn) return fail(res, 401, "unauthenticated", "Sign in first.");

    // ---- API ----
    if (pathname === "/api/workspaces" && method === "GET") {
      if (!dockerAvailable)
        return fail(res, 503, "docker-unavailable", "The Docker daemon is not reachable.");
      return json(res, 200, {
        workspaces: workspaces.map((w) => {
          const active = [...jobs.values()].find(
            (j) => j.workspace === w.name && j.finishedAt === null,
          );
          return {
            ...w,
            activeJob: active
              ? { jobId: active.jobId, operation: active.operation, state: active.state }
              : null,
          };
        }),
      });
    }
    if (pathname === "/api/templates" && method === "GET") return json(res, 200, { templates });
    if (pathname === "/api/jobs" && method === "GET") {
      return json(res, 200, { jobs: [...jobs.values()].reverse().map(summary) });
    }
    if (pathname === "/api/jobs" && method === "POST") {
      const input = JSON.parse(await readBody(req)) as {
        operation?: string;
        workspace?: string;
        templateId?: string;
      };
      const { operation, workspace: name } = input;
      if (
        !operation ||
        !name ||
        !["create", "upgrade", "start", "stop", "rebuild"].includes(operation)
      ) {
        return fail(res, 400, "invalid-request", "operation and workspace are required");
      }
      if (
        (operation === "create" || operation === "upgrade") &&
        !templates.some((t) => t.id === input.templateId)
      ) {
        return fail(res, 404, "unknown-template", `no approved template ${input.templateId}`);
      }
      const ws = workspace(name);
      if (operation === "create" && ws)
        return fail(res, 409, "workspace-exists", `workspace ${name} already exists`);
      if (operation !== "create" && !ws)
        return fail(res, 404, "unknown-workspace", `no workspace ${name}`);
      const active = [...jobs.values()].find((j) => j.workspace === name && j.finishedAt === null);
      if (active) {
        return fail(
          res,
          409,
          "workspace-busy",
          `workspace ${name} already has a ${active.operation} job`,
          {
            activeJobId: active.jobId,
          },
        );
      }
      sequence += 1;
      const job: FixtureJob = {
        jobId: `job-${sequence}-${randomUUID().slice(0, 8)}`,
        workspace: name,
        operation,
        state: "running",
        phase: "queued",
        createdAt: new Date(Date.UTC(2026, 0, 1, 9, 0, sequence)).toISOString(),
        finishedAt: null,
        error: null,
        events: [],
        listeners: new Set(),
        sentSession: null,
        step: 0,
        release: null,
        answerCount: 0,
        stage: "first",
      };
      if (input.templateId) job.templateId = input.templateId;
      jobs.set(job.jobId, job);
      setImmediate(() => {
        if (operation === "create") void runCreate(job);
        else if (operation === "upgrade") void runUpgrade(job);
        else void runContainerJob(job);
      });
      return json(res, 202, summary(job), { Location: `/api/jobs/${job.jobId}` });
    }
    const jobMatch = /^\/api\/jobs\/([^/]+)(\/events|\/answers)?$/.exec(pathname);
    if (jobMatch) {
      const job = jobs.get(decodeURIComponent(jobMatch[1] ?? ""));
      if (!job) return fail(res, 404, "job-not-found", "no such job");
      if (!jobMatch[2] && method === "GET") return json(res, 200, summary(job));
      if (jobMatch[2] === "/events" && method === "GET") {
        const after =
          Number(req.headers["last-event-id"] ?? url.searchParams.get("lastEventId") ?? 0) || 0;
        res.writeHead(200, {
          "Content-Type": "text/event-stream",
          "Cache-Control": "no-store",
          Connection: "keep-alive",
        });
        res.flushHeaders();
        for (const event of job.events.filter((e) => e.id > after)) send(res, event);
        if (job.finishedAt !== null) return res.end();
        job.listeners.add(res);
        req.on("close", () => job.listeners.delete(res));
        return;
      }
      if (jobMatch[2] === "/answers" && method === "POST") {
        const body = await readBody(req);
        let parsed: { submission?: { template: string; answers: Record<string, unknown> } };
        try {
          parsed = JSON.parse(body) as typeof parsed;
        } catch {
          return fail(res, 400, "invalid-request", "body is not JSON");
        }
        if (job.state !== "waiting-for-answers" || job.sentSession === null) {
          return fail(res, 409, "job-not-waiting", "the job is not waiting for answers");
        }
        if (!body.includes(`"session":${job.sentSession}`)) {
          return fail(res, 400, "session-changed", "the session did not come back byte for byte");
        }
        const submission = parsed.submission;
        if (
          !submission ||
          typeof submission.template !== "string" ||
          typeof submission.answers !== "object"
        ) {
          return fail(res, 400, "invalid-request", "submission must be {template, answers}");
        }
        const reply = answer(job, submission);
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(reply);
        return;
      }
    }
    return fail(res, 404, "not-found", pathname);
  }

  const server = createServer((req, res) => {
    handle(req, res).catch((error: unknown) => {
      if (!res.headersSent) fail(res, 500, "fixture-fault", String(error));
      else res.end();
    });
  });

  return {
    server,
    listen(port: number, host = "127.0.0.1") {
      return new Promise<string>((resolve) => {
        server.listen(port, host, () => resolve(`http://${host}:${port}`));
      });
    },
    close() {
      reset();
      return new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}

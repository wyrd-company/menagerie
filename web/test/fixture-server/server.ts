// Test-only fixture of the workspace service's browser contract
// (docs/specifications/workspace-api.yml and web/docs/interview-jobs.yml).
// It serves the built UI and answers /api with in-memory workspaces and jobs.
// Interviews replay the Toha documents in test/fixtures/toha/documents, which
// a real Toha build produced.
//
// It is not part of Menagerie: no OIDC, no Docker, no Toha. Routes under
// /__fixture let browser tests steer it.
import { randomUUID } from "node:crypto";
import { createReadStream, existsSync, statSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { extname, join, normalize } from "node:path";
import { send, summary } from "./events.ts";
import { answer, run } from "./scenarios.ts";
import {
  initialWorkspaces,
  templates,
  type FixtureJob,
  type FixtureOptions,
  type World,
} from "./state.ts";

export function createFixture(options: { distDir: string }) {
  const world: World = { workspaces: initialWorkspaces(), options: { upgradeDocument: "raw" } };
  let jobs = new Map<string, FixtureJob>();
  let signedIn = true;
  let dockerAvailable = true;
  let sequence = 0;

  const reset = () => {
    for (const job of jobs.values()) for (const res of job.listeners) res.end();
    world.workspaces = initialWorkspaces();
    world.options = { upgradeDocument: "raw" };
    jobs = new Map();
    signedIn = true;
    dockerAvailable = true;
    sequence = 0;
  };

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
        world.options = { ...world.options, ...(input as Partial<FixtureOptions>) };
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
        workspaces: world.workspaces.map((w) => {
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
      const ws = world.workspaces.find((w) => w.name === name);
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
        run(world, job);
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
        // A short reconnect delay makes a client that keeps listening after the result visible.
        res.write("retry: 50\n\n");
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
        const reply = answer(world, job, submission);
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

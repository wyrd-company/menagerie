// Test-only fixture of the workspace service's browser contract
// (docs/specifications/workspace-api.yml and its OpenAPI document).
// It serves the built UI and answers /api with in-memory workspaces and jobs.
// Interviews replay the Toha documents in test/fixtures/toha/documents, which
// a real Toha build produced.
//
// It is not part of Menagerie: no OIDC, no Docker, no Toha. Routes under
// /__fixture let browser tests steer it.
import type { PolicySettings } from "../../src/api/types.ts";
import { randomUUID } from "node:crypto";
import { createReadStream, existsSync, statSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { extname, join, normalize } from "node:path";
import { finish, log, send, summary } from "./events.ts";
import { answer, run } from "./scenarios.ts";
import {
  initialTemplates,
  initialWorkspaces,
  newer,
  type FixtureJob,
  type FixtureOptions,
  type FixtureTemplate,
  type World,
} from "./state.ts";

export function createFixture(options: { distDir: string }) {
  const world: World = {
    workspaces: initialWorkspaces(),
    templates: initialTemplates(),
    options: { upgradeDocument: "raw", t3CodeInstalled: true },
  };
  let jobs = new Map<string, FixtureJob>();
  let signedIn = true;
  let dockerAvailable = true;
  let sequence = 0;
  let policySettings: PolicySettings = { required: [], groups: [], revision: 0 };
  let nextGroupID = 1;
  let selections = new Map<string, string[]>();
  const revisions = new Map<string, number>();
  const applied = new Map<string, number>();
  const policyNames = ["workspace", "xolotl-client-key", "bob-ssh", "ira-ssh"];

  const reset = () => {
    for (const job of jobs.values()) for (const res of job.listeners) res.end();
    world.workspaces = initialWorkspaces();
    world.templates = initialTemplates();
    world.options = { upgradeDocument: "raw", t3CodeInstalled: true };
    jobs = new Map();
    signedIn = true;
    dockerAvailable = true;
    sequence = 0;
    policySettings = { required: [], groups: [], revision: 0 };
    nextGroupID = 1;
    selections = new Map();
    revisions.clear();
    applied.clear();
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
    if (pathname === "/api/policies" && method === "GET") {
      return json(res, 200, {
        grant: { agents: ["sample-host"], policies: ["*"], actions: ["read", "bind"] },
        policies: policyNames.map((name) => ({ name, valueNames: [], inGrant: true })),
        settings: policySettings,
      });
    }
    if (pathname === "/api/policies/settings" && method === "PUT") {
      const settings = JSON.parse(await readBody(req)) as PolicySettings;
      if (settings.revision !== policySettings.revision)
        return fail(
          res,
          409,
          "settings-changed",
          "Policy settings changed. Reload them and try again.",
        );
      const assigned = [...settings.required, ...settings.groups.flatMap((g) => g.policies)];
      const outside = assigned.filter((p) => !policyNames.includes(p));
      if (outside.length)
        return fail(
          res,
          422,
          "policy-outside-grant",
          `Policies outside grant: ${outside.join(", ")}`,
        );
      if (new Set(assigned).size !== assigned.length)
        return fail(
          res,
          422,
          "invalid-settings",
          "A policy is required, in one group, or neither.",
        );
      if (new Set(settings.groups.map((g) => g.name)).size !== settings.groups.length)
        return fail(res, 422, "invalid-settings", "Group names must be unique.");
      for (const group of settings.groups) {
        if (!group.name.trim() || group.name.length > 100 || /[\r\n]/.test(group.name))
          return fail(
            res,
            422,
            "invalid-settings",
            "A group name must be one line of 1 to 100 characters.",
          );
        if (!["single", "multi"].includes(group.mode))
          return fail(res, 422, "invalid-settings", `Group ${group.name} has unknown mode.`);
        if (!group.policies.length)
          return fail(res, 422, "invalid-settings", `Group ${group.name} has no policy.`);
        if (group.id !== undefined && !policySettings.groups.some((g) => g.id === group.id))
          return fail(res, 422, "invalid-settings", `Group ${group.name} has unknown id.`);
      }
      policySettings = {
        ...settings,
        revision: settings.revision + 1,
        groups: settings.groups.map((g) => ({ ...g, id: g.id ?? nextGroupID++ })),
      };
      for (const workspace of world.workspaces)
        selections.set(
          workspace.name,
          [...new Set([...(selections.get(workspace.name) ?? []), ...settings.required])].sort(),
        );
      return json(res, 200, policySettings);
    }

    if (pathname === "/api/workspaces" && method === "GET") {
      if (!dockerAvailable)
        return fail(res, 503, "docker-unavailable", "The Docker daemon is not reachable.");
      return json(res, 200, {
        workspaces: world.workspaces.map((w) => {
          const active = [...jobs.values()].find(
            (j) => j.workspace === w.name && j.finishedAt === null,
          );
          const template = world.templates.find((t) => t.id === w.template);
          return {
            name: w.name,
            state: w.state,
            containerId: w.containerId,
            template: template ? { id: template.id, name: template.name } : null,
            policies: selections.get(w.name) ?? [],
            selectionRevision: revisions.get(w.name) ?? 0,
            appliedRevision: applied.get(w.name) ?? 0,
            selectionImported: true,
            applyFailure: null,
            xolotlKey: null,
            needsChoice: policySettings.groups
              .filter((g) => {
                const count = g.policies.filter((p) =>
                  (selections.get(w.name) ?? []).includes(p),
                ).length;
                return count === 0 || (g.mode === "single" && count !== 1);
              })
              .map((g) => g.name),
            activeJob: active
              ? { jobId: active.jobId, operation: active.operation, state: active.state }
              : null,
          };
        }),
      });
    }
    const selectionMatch = /^\/api\/workspaces\/([^/]+)\/policies$/.exec(pathname);
    if (selectionMatch && method === "PUT") {
      const name = decodeURIComponent(selectionMatch[1]!);
      const ws = world.workspaces.find((w) => w.name === name && w.state !== "no-folder");
      if (!ws) return fail(res, 404, "workspace-not-found", `no workspace ${name}`);
      const input = JSON.parse(await readBody(req)) as { policies: string[]; revision: number };
      if (input.revision !== (revisions.get(name) ?? 0))
        return fail(
          res,
          409,
          "selection-changed",
          "Policy selection changed. Reload and try again.",
        );
      if (policySettings.required.some((p) => !input.policies.includes(p)))
        return fail(res, 422, "policy-required", "Required policies cannot be removed.");
      if (input.policies.some((p) => !policyNames.includes(p)))
        return fail(res, 422, "policy-outside-grant", "Policy outside grant.");
      selections.set(name, input.policies);
      const revision = input.revision + 1;
      revisions.set(name, revision);
      applied.set(name, revision);
      return json(res, 200, { selectionRevision: revision, jobId: null });
    }
    const pairing = /^\/api\/workspaces\/([^/]+)\/t3-code-pairing-tokens$/.exec(pathname);
    if (pairing && method === "POST") {
      const name = decodeURIComponent(pairing[1]!);
      const ws = world.workspaces.find((w) => w.name === name);
      if (!ws) return fail(res, 404, "workspace-not-found", `no workspace ${name}`);
      if (ws.state !== "running")
        return fail(res, 409, "workspace-not-running", "this workspace's container is not running");
      if (!world.options.t3CodeInstalled)
        return fail(
          res,
          409,
          "t3-code-not-installed",
          "T3 Code is not installed in this workspace's container",
        );
      return json(res, 201, {
        token: "SAMPLETOKEN123",
        expiresAt: new Date(Date.now() + 5 * 60_000).toISOString(),
      });
    }
    const templateBody = (t: FixtureTemplate) => ({
      id: t.id,
      name: t.name,
      address: t.address,
      currentVersion: t.currentVersion,
      runnerImage: t.runnerImage,
      workspaces: world.workspaces.filter((w) => w.template === t.id).map((w) => w.name),
    });
    const validName = (name: unknown) =>
      typeof name === "string" &&
      name.trim() !== "" &&
      [...name].length <= 100 &&
      ![...name].some((c) => c.charCodeAt(0) < 0x20 || "\u007f\u2028\u2029".includes(c));
    if (pathname === "/api/templates" && method === "GET") {
      const sorted = [...world.templates].sort(
        (a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id),
      );
      return json(res, 200, { templates: sorted.map(templateBody) });
    }
    if (pathname === "/api/templates" && method === "POST") {
      // The fixture's checks stand in for the template service's: an address
      // naming a repository below gives that refusal, by its check.
      const input = JSON.parse(await readBody(req)) as {
        name?: unknown;
        address?: unknown;
        runnerImage?: unknown;
      };
      if (!validName(input.name) || typeof input.address !== "string")
        return fail(res, 400, "invalid-request", "name must be one line of 1 to 100 characters");
      const address = input.address;
      const check = (name: string, message: string) =>
        fail(res, 422, "template-check-failed", message, { check: name });
      if (!/^(gh:|git@|ssh:\/\/|https:\/\/)/.test(address) || /:\/\/[^/@]*:[^/@]*@/.test(address))
        return check(
          "address",
          "the address is refused: it must be gh:owner/repository, git@host:path, ssh:// or https://, with no password",
        );
      if (address.includes("missing"))
        return check(
          "fetch",
          "the template could not be fetched: the Git host has no such repository",
        );
      if (address.includes("untagged"))
        return check(
          "version",
          "the repository has no version: a version is a tag MAJOR.MINOR.PATCH, with an optional leading v",
        );
      if (address.includes("bare"))
        return check(
          "definition",
          "the template's files at version 1.0.0 have no .devcontainer/devcontainer.json",
        );
      const base = address.replace(/@[^@/#]*(#|$)/, "$1");
      const existing = world.templates.find((t) => t.address === base);
      if (existing)
        return fail(
          res,
          409,
          "template-exists",
          `this template is registered already, as ${existing.name} (${existing.id})`,
        );
      const stem =
        (input.name as string)
          .toLowerCase()
          .split(/[^a-z0-9]+/)
          .filter(Boolean)
          .join("-") || "template";
      let id = stem;
      for (let n = 2; world.templates.some((t) => t.id === id); n++) id = `${stem}-${n}`;
      const template: FixtureTemplate = {
        id,
        name: (input.name as string).trim(),
        address: base,
        currentVersion: "1.0.0",
        runnerImage: typeof input.runnerImage === "string" ? input.runnerImage : null,
        versions: ["1.0.0", "1.1.0"],
      };
      world.templates.push(template);
      return json(res, 201, templateBody(template), { Location: `/api/templates/${id}` });
    }
    const templateMatch = /^\/api\/templates\/([^/]+)(\/upgrades)?$/.exec(pathname);
    if (templateMatch) {
      const template = world.templates.find((t) => t.id === decodeURIComponent(templateMatch[1]!));
      if (!template)
        return fail(res, 404, "unknown-template", "no registered template has this id");
      if (templateMatch[2] && method === "POST") {
        const { version } = JSON.parse(await readBody(req)) as { version?: string };
        if (
          !version ||
          !template.versions.includes(version) ||
          !newer(version, template.currentVersion)
        )
          return fail(
            res,
            409,
            "version-not-newer",
            "the version is not a version of the template's repository newer than its current version",
          );
        template.currentVersion = version;
        return json(res, 200, templateBody(template));
      }
      if (!templateMatch[2] && method === "PATCH") {
        const edit = JSON.parse(await readBody(req)) as { name?: unknown; runnerImage?: unknown };
        if (edit.name !== undefined && !validName(edit.name))
          return fail(res, 400, "invalid-request", "name must be one line of 1 to 100 characters");
        if (typeof edit.name === "string") template.name = edit.name;
        if (edit.runnerImage !== undefined)
          template.runnerImage = typeof edit.runnerImage === "string" ? edit.runnerImage : null;
        return json(res, 200, templateBody(template));
      }
      if (!templateMatch[2] && method === "DELETE") {
        const users = templateBody(template).workspaces;
        if (users.length)
          return fail(
            res,
            409,
            "template-in-use",
            `the template is used by workspace ${users.join(", ")}; delete those workspaces first`,
          );
        world.templates = world.templates.filter((t) => t !== template);
        res.writeHead(204);
        res.end();
        return;
      }
    }
    if (pathname === "/api/jobs" && method === "GET") {
      return json(res, 200, { jobs: [...jobs.values()].reverse().map(summary) });
    }
    if (pathname === "/api/jobs" && method === "POST") {
      const input = JSON.parse(await readBody(req)) as {
        operation?: string;
        workspace?: string;
        templateId?: string;
        branch?: string;
        policies?: string[];
      };
      const { operation, workspace: name } = input;
      if (
        !operation ||
        !name ||
        ![
          "create",
          "upgrade",
          "start",
          "stop",
          "rebuild",
          "apply-branch",
          "delete",
          "apply-policies",
        ].includes(operation)
      ) {
        return fail(res, 400, "invalid-request", "operation and workspace are required");
      }
      if (operation === "apply-branch" && !input.branch) {
        return fail(res, 400, "invalid-request", "branch is required");
      }
      if (operation === "create" && !world.templates.some((t) => t.id === input.templateId)) {
        return fail(res, 404, "unknown-template", `no registered template ${input.templateId}`);
      }
      if (operation === "upgrade" && input.templateId !== undefined) {
        return fail(res, 400, "invalid-request", "an upgrade takes no templateId");
      }
      if (operation === "create") {
        const chosen = [...new Set([...(input.policies ?? []), ...policySettings.required])];
        if (
          policySettings.groups.some((g) => {
            const n = g.policies.filter((p) => chosen.includes(p)).length;
            return n === 0 || (g.mode === "single" && n !== 1);
          })
        )
          return fail(res, 422, "policy-choice-required", "Choose a policy in each group.");
        selections.set(name, chosen);
        revisions.set(name, 1);
        applied.set(name, 1);
      }
      const ws = world.workspaces.find((w) => w.name === name);
      if (operation === "upgrade" && ws && ws.state !== "no-folder") {
        const template = world.templates.find((t) => t.id === ws.template);
        if (!template)
          return fail(
            res,
            409,
            "template-not-registered",
            `workspace ${name} has no registered template`,
          );
        if (ws.applied && !newer(template.currentVersion, ws.applied))
          return fail(
            res,
            409,
            "upgrade-not-needed",
            `workspace ${name} already has version ${ws.applied} of template ${template.id}, whose current version is ${template.currentVersion}`,
          );
      }
      if (operation === "create" && ws)
        return fail(res, 409, "workspace-exists", `workspace ${name} already exists`);
      if (operation !== "create" && (!ws || (ws.state === "no-folder" && operation !== "delete")))
        return fail(res, 404, "workspace-not-found", `no workspace ${name}`);
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
      if (input.branch) job.branch = input.branch;
      jobs.set(job.jobId, job);
      setImmediate(() => {
        if (operation === "apply-policies") {
          applied.set(name, revisions.get(name) ?? 0);
          log(job, "policies applied");
          finish(job, "succeeded");
        } else run(world, job);
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
          return fail(res, 409, "session-mismatch", "the session did not come back byte for byte");
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

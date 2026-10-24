// @vitest-environment node
//
// Holds the test-only fixture server, and the requests the UI client sends,
// to the workspace service's published contract
// (docs/specifications/assets/workspace-api/openapi.yaml). A fixture that
// drifts from the real service fails here, not in the browser.
import { readFileSync } from "node:fs";
import { mkdtempSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Ajv2020 } from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { parse } from "yaml";
import { answerJob, parseInterviewReply, startJob } from "../../src/api/client.ts";
import { createFixture } from "../fixture-server/server.ts";

const specPath = new URL(
  "../../../docs/specifications/assets/workspace-api/openapi.yaml",
  import.meta.url,
);
const spec = parse(readFileSync(specPath, "utf8")) as {
  paths: Record<string, Record<string, { responses: Record<string, unknown> }>>;
};

const ajv = new Ajv2020({ strict: false, allErrors: true });
addFormats.default(ajv);
ajv.addSchema(spec as object, "openapi");

const pointer = (...parts: string[]) =>
  "openapi#/" + parts.map((p) => p.replace(/~/g, "~0").replace(/\//g, "~1")).join("/");

function expectValid(ref: string, value: unknown) {
  const validate = ajv.getSchema(ref) ?? ajv.compile({ $ref: ref });
  const ok = validate(value);
  expect(ok, `${ref}\n${JSON.stringify(validate.errors, null, 2)}\n${JSON.stringify(value)}`).toBe(
    true,
  );
}

/** Validates a response against the operation that declares its status. */
async function expectResponse(path: string, method: string, response: Response): Promise<unknown> {
  const status = String(response.status);
  const operation = spec.paths[path]?.[method];
  expect(operation, `${method} ${path} is not in the contract`).toBeDefined();
  expect(
    Object.keys(operation!.responses),
    `${method} ${path} does not declare ${status}`,
  ).toContain(status);
  const body = (await response.json()) as unknown;
  const declared = operation!.responses[status] as { $ref?: string };
  const ref = declared.$ref
    ? pointer("components", "schemas", "ErrorEnvelope")
    : pointer("paths", path, method, "responses", status, "content", "application/json", "schema");
  expectValid(ref, body);
  return body;
}

const schema = (name: string) => pointer("components", "schemas", name);

let base = "";
const fixture = createFixture({ distDir: mkdtempSync(join(tmpdir(), "contract-dist-")) });

beforeAll(async () => {
  await new Promise<void>((resolve) => fixture.server.listen(0, "127.0.0.1", resolve));
  base = `http://127.0.0.1:${(fixture.server.address() as AddressInfo).port}`;
});
afterAll(() => fixture.close());

const call = (path: string, init?: RequestInit) => fetch(base + path, init);
const post = (path: string, body: unknown) =>
  call(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });

/** Reads a job's event stream until `done` holds or the stream ends. */
async function readEvents(jobId: string, done: (types: string[]) => boolean) {
  const controller = new AbortController();
  const response = await call(`/api/jobs/${jobId}/events`, { signal: controller.signal });
  expect(response.headers.get("content-type")).toContain("text/event-stream");
  const reader = response.body!.pipeThrough(new TextDecoderStream()).getReader();
  const events: { id: string; type: string; data: string }[] = [];
  let buffer = "";
  while (!done(events.map((e) => e.type))) {
    const { value, done: ended } = await reader.read();
    if (ended) break;
    buffer += value;
    let end: number;
    while ((end = buffer.indexOf("\n\n")) >= 0) {
      const block = buffer.slice(0, end);
      buffer = buffer.slice(end + 2);
      const lines = block.split("\n");
      const field = (name: string) =>
        lines.filter((l) => l.startsWith(`${name}: `)).map((l) => l.slice(name.length + 2));
      if (field("event").length === 0) continue; // a retry line
      events.push({
        id: field("id")[0]!,
        type: field("event")[0]!,
        data: field("data").join("\n"),
      });
    }
  }
  controller.abort();
  return events;
}

function expectEvents(events: { type: string; data: string }[]) {
  const names: Record<string, string> = {
    state: "StateEvent",
    log: "LogEvent",
    interview: "InterviewReply",
    result: "ResultEvent",
  };
  for (const event of events) {
    expect(Object.keys(names), `undeclared event type ${event.type}`).toContain(event.type);
    expect(event.data.includes("\n"), `${event.type} data spans lines`).toBe(false);
    expectValid(schema(names[event.type]!), JSON.parse(event.data));
  }
}

describe("the fixture server keeps the workspace API contract", () => {
  it("lists workspaces, templates and jobs", async () => {
    await post("/__fixture/reset", {});
    await expectResponse("/api/workspaces", "get", await call("/api/workspaces"));
    await expectResponse("/api/templates", "get", await call("/api/templates"));
    await expectResponse("/api/jobs", "get", await call("/api/jobs"));
  });

  it("starts a container job, streams it and refuses a second job for the workspace", async () => {
    await post("/__fixture/reset", {});
    const started = await post("/api/jobs", { operation: "rebuild", workspace: "sample-slow" });
    expect(started.headers.get("location")).toMatch(/^\/api\/jobs\/.+/);
    const job = (await expectResponse("/api/jobs", "post", started)) as { jobId: string };
    const busy = await expectResponse(
      "/api/jobs",
      "post",
      await post("/api/jobs", { operation: "stop", workspace: "sample-slow" }),
    );
    expect(busy).toMatchObject({ error: { kind: "workspace-busy", activeJobId: job.jobId } });
    await expectResponse("/api/jobs/{id}", "get", await call(`/api/jobs/${job.jobId}`));
    await post(`/__fixture/release/${job.jobId}`, {});
    expectEvents(await readEvents(job.jobId, (types) => types.includes("result")));
  });

  it("answers a failing job's result with an error kind the contract names", async () => {
    await post("/__fixture/reset", {});
    const job = (await (
      await post("/api/jobs", { operation: "start", workspace: "sample-broken" })
    ).json()) as { jobId: string };
    const events = await readEvents(job.jobId, (types) => types.includes("result"));
    expectEvents(events);
    expect(JSON.parse(events.at(-1)!.data)).toMatchObject({
      state: "failed",
      error: { kind: "initialize-command-failed" },
    });
  });

  it("refuses unknown jobs, workspaces, templates and malformed requests with error envelopes", async () => {
    await post("/__fixture/reset", {});
    const kinds = [
      [
        await expectResponse("/api/jobs/{id}", "get", await call("/api/jobs/missing")),
        "job-not-found",
      ],
      [
        await expectResponse(
          "/api/jobs",
          "post",
          await post("/api/jobs", { operation: "start", workspace: "sample-missing" }),
        ),
        "workspace-not-found",
      ],
      [
        await expectResponse(
          "/api/jobs",
          "post",
          await post("/api/jobs", {
            operation: "create",
            workspace: "sample-new",
            templateId: "nope",
          }),
        ),
        "unknown-template",
      ],
      [
        await expectResponse(
          "/api/jobs",
          "post",
          await post("/api/jobs", {
            operation: "create",
            workspace: "sample-alpha",
            templateId: "sample-kit-v1",
          }),
        ),
        "workspace-exists",
      ],
      [
        await expectResponse(
          "/api/jobs",
          "post",
          await post("/api/jobs", { workspace: "sample-alpha" }),
        ),
        "invalid-request",
      ],
    ] as const;
    for (const [body, kind] of kinds) expect(body).toMatchObject({ error: { kind } });
  });

  it("runs a create interview: replies, rejection, mismatch, completion and result", async () => {
    await post("/__fixture/reset", {});
    const job = (await (
      await post("/api/jobs", {
        operation: "create",
        workspace: "sample-space",
        templateId: "sample-kit-v1",
      })
    ).json()) as { jobId: string };
    const first = await readEvents(job.jobId, (types) => types.includes("interview"));
    expectEvents(first);
    const reply = parseInterviewReply(first.find((e) => e.type === "interview")!.data);
    const template = reply.document?.context.template ?? "";
    const answer = (session: string, answers: object) =>
      call(`/api/jobs/${job.jobId}/answers`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: `{"session":${session},"submission":${JSON.stringify({ template, answers })}}`,
      });
    // Each reply is read as text so the next answer carries its session as received.
    const reply200 = async (response: Response) => {
      const text = await response.clone().text();
      await expectResponse("/api/jobs/{id}/answers", "post", response);
      return parseInterviewReply(text);
    };
    const mismatch = await expectResponse(
      "/api/jobs/{id}/answers",
      "post",
      await answer('{"other":1}', {}),
    );
    expect(mismatch).toMatchObject({ error: { kind: "session-mismatch" } });
    const rejected = await reply200(await answer(reply.session, { labels: ["Bad Label"] }));
    expect(rejected.document).toMatchObject({
      status: "questions",
      errors: { labels: expect.any(Array) },
    });
    const second = await reply200(await answer(rejected.session, { title: "Sample project" }));
    expect(second.state).toBe("questions");
    const done = await reply200(await answer(second.session, {}));
    expect(done).toMatchObject({ state: "completed", document: null });
    const notWaiting = await expectResponse(
      "/api/jobs/{id}/answers",
      "post",
      await answer(done.session, {}),
    );
    expect(notWaiting).toMatchObject({ error: { kind: "job-not-waiting" } });
    const all = await readEvents(job.jobId, (types) => types.includes("result"));
    expectEvents(all);
    expect(JSON.parse(all.at(-1)!.data)).toMatchObject({
      state: "succeeded",
      apply: { outcome: "written" },
    });
  });

  it("answers a conflicted upgrade with the contract's result and apply", async () => {
    await post("/__fixture/reset", {});
    await post("/__fixture/options", { upgradeDocument: "uncovered" });
    const job = (await (
      await post("/api/jobs", {
        operation: "upgrade",
        workspace: "sample-beta",
        templateId: "sample-kit-v2",
      })
    ).json()) as { jobId: string };
    const events = await readEvents(job.jobId, (types) => types.includes("interview"));
    const reply = parseInterviewReply(events.find((e) => e.type === "interview")!.data);
    await call(`/api/jobs/${job.jobId}/answers`, {
      method: "POST",
      body: `{"session":${reply.session},"submission":${JSON.stringify({ template: reply.document?.context.template, answers: { region: "north" } })}}`,
    });
    const all = await readEvents(job.jobId, (types) => types.includes("result"));
    expectEvents(all);
    expect(JSON.parse(all.at(-1)!.data)).toMatchObject({
      state: "failed",
      error: { kind: "merge-conflicted" },
      apply: { outcome: "conflicted" },
    });
  });

  it("answers a signed-out API request with 401 unauthenticated", async () => {
    await post("/__fixture/reset", {});
    await post("/__fixture/auth", { signedIn: false });
    const body = await (await call("/api/workspaces")).json();
    expectValid(schema("ErrorEnvelope"), body);
    expect(body).toMatchObject({ error: { kind: "unauthenticated" } });
    await post("/__fixture/auth", { signedIn: true });
  });
});

describe("the UI client sends request bodies the contract accepts", () => {
  it("starts every operation with a JobRequest", async () => {
    const bodies: unknown[] = [];
    vi.stubGlobal("fetch", async (_: string, init: RequestInit) => {
      bodies.push(JSON.parse(init.body as string));
      return Response.json({}, { status: 202 });
    });
    try {
      for (const operation of ["start", "stop", "rebuild"] as const)
        await startJob({ operation, workspace: "sample" });
      for (const operation of ["create", "upgrade"] as const)
        await startJob({ operation, workspace: "sample", templateId: "sample-kit" });
    } finally {
      vi.unstubAllGlobals();
    }
    for (const body of bodies) expectValid(schema("JobRequest"), body);
    expect(bodies).toHaveLength(5);
  });

  it("answers with an AnswerRequest that carries the session as received", async () => {
    let sent = "";
    vi.stubGlobal("fetch", async (_: string, init: RequestInit) => {
      sent = init.body as string;
      return new Response("", { status: 200 });
    });
    const session = '{"counter":12345678901234567890,"ratio":1.0}';
    try {
      await answerJob(
        "job-1",
        parseInterviewReply(
          `{"session":${session},"state":"questions","document":null,"warnings":[]}`,
        ).session,
        {
          template: "sample",
          answers: { title: "Sample" },
        },
      );
    } finally {
      vi.unstubAllGlobals();
    }
    expectValid(schema("AnswerRequest"), JSON.parse(sent));
    expect(sent).toBe(
      `{"session":${session},"submission":{"template":"sample","answers":{"title":"Sample"}}}`,
    );
  });
});

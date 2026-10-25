import { describe, expect, it } from "vitest";
import type { Job, JobEvent } from "../api/types.ts";
import rejected from "../../test/fixtures/toha/documents/create-batch-1-rejected.json";
import batch1 from "../../test/fixtures/toha/documents/create-batch-1.json";
import batch2 from "../../test/fixtures/toha/documents/create-batch-2.json";
import { decodeEvent, initialJobView, isTerminal, jobReducer, type JobView } from "./jobView.ts";

const session = '{"counter":12345678901234567890,"ratio":1.0}';
const reply = (doc: unknown, state = "questions") =>
  `{"session":${session},"state":"${state}","document":${JSON.stringify(doc)},"warnings":[]}`;

function fold(events: [string, string][], start: JobView = initialJobView): JobView {
  return events.reduce(
    (view, [type, data], i) =>
      jobReducer(view, { type: "event", event: decodeEvent(type, String(i + 1), data) }),
    start,
  );
}

const job: Job = {
  jobId: "job-1",
  workspace: "sample-space",
  operation: "create",
  state: "waiting-for-answers",
  phase: "interview",
  createdAt: "2026-01-01T00:00:00Z",
  finishedAt: null,
  lastEventId: 3,
  error: null,
};

describe("jobReducer", () => {
  it("records phases in stream order and log lines", () => {
    const view = fold([
      ["state", '{"state":"running","phase":"template"}'],
      ["log", '{"text":"resolving"}'],
      ["state", '{"state":"waiting-for-answers","phase":"interview"}'],
    ]);
    expect(view.phases).toEqual(["template", "interview"]);
    expect(view.state).toBe("waiting-for-answers");
    expect(view.logs).toEqual([{ id: 2, text: "resolving" }]);
    expect(view.lastEventId).toBe(3);
  });

  it("ignores an event it already applied, so a replay cannot double a log line", () => {
    const view = fold([["log", '{"text":"one"}']]);
    const again = jobReducer(view, {
      type: "event",
      event: decodeEvent("log", "1", '{"text":"one"}'),
    });
    expect(again).toBe(view);
  });

  it("lets the summary set state and phase only before the stream speaks", () => {
    const fresh = jobReducer(initialJobView, { type: "summary", job });
    expect(fresh).toMatchObject({ state: "waiting-for-answers", phase: "interview", phases: [] });
    const streamed = fold([["state", '{"state":"running","phase":"template"}']]);
    expect(jobReducer(streamed, { type: "summary", job }).state).toBe("running");
  });

  it("keeps the session raw and counts batches, not rejections", () => {
    let view = fold([["interview", reply(batch1)]]);
    expect(view.interview?.session).toBe(session);
    expect(view.batch).toBe(1);
    view = jobReducer(view, {
      type: "interview",
      reply: decodeEvent("interview", "9", reply(rejected)).data as never,
    });
    expect(view.batch).toBe(1);
    expect(view.interview?.document).toEqual(rejected);
    view = jobReducer(view, {
      type: "interview",
      reply: decodeEvent("interview", "9", reply(batch2)).data as never,
    });
    expect(view.batch).toBe(2);
  });

  it("ignores an identical reply arriving from both the answers response and the stream", () => {
    const decoded = decodeEvent("interview", "1", reply(batch2)) as Extract<
      JobEvent,
      { type: "interview" }
    >;
    const view = jobReducer(initialJobView, { type: "interview", reply: decoded.data });
    expect(jobReducer(view, { type: "event", event: decoded })).toMatchObject({
      batch: 1,
      lastEventId: 1,
    });
    expect(jobReducer(view, { type: "event", event: decoded }).interview).toBe(view.interview);
  });

  it("ends at the result event", () => {
    const view = fold([
      ["result", '{"state":"failed","error":{"kind":"initialize-command","message":"exit 3"}}'],
    ]);
    expect(isTerminal(view)).toBe(true);
    expect(view.result?.error?.kind).toBe("initialize-command");
  });

  it("shows unknown events as log lines", () => {
    expect(fold([["progress", '{"n":1}']]).logs).toEqual([{ id: 1, text: '[progress] {"n":1}' }]);
  });
});

describe("decodeEvent", () => {
  it("keeps a payload that does not decode as a visible line", () => {
    expect(decodeEvent("state", "4", "{not json")).toEqual({
      id: 4,
      type: "unknown",
      event: "state",
      data: "{not json",
    });
    expect(fold([["interview", '{"state":"questions"}']]).logs[0]?.text).toBe(
      '[interview] {"state":"questions"}',
    );
  });
});

describe("interview replies keep the document text", () => {
  it("keeps the document's raw text for its defaults", () => {
    const text = `{"session":{},"state":"questions","document":{"schema":{"properties":{"d":{"default":9007199254740993}}}},"warnings":[]}`;
    const event = decodeEvent("interview", "1", text) as Extract<JobEvent, { type: "interview" }>;
    expect(event.data.documentText).toBe(
      '{"schema":{"properties":{"d":{"default":9007199254740993}}}}',
    );
    expect(
      decodeEvent(
        "interview",
        "2",
        `{"session":{},"state":"completed","document":null,"warnings":[]}`,
      ).data,
    ).toMatchObject({ documentText: null });
  });
});

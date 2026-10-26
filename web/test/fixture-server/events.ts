// A job's event history and the SSE encoding of it.
import { readFileSync } from "node:fs";
import type { ServerResponse } from "node:http";
import type { FixtureJob, StoredEvent } from "./state.ts";

const documentsDir = new URL("../fixtures/toha/documents/", import.meta.url);

/**
 * Removes whitespace outside strings, as the workspace service compacts each
 * event payload to one line without changing its text, numbers included.
 */
export function compactJson(text: string): string {
  let out = "";
  let inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i]!;
    if (inString) {
      out += c;
      if (c === "\\") out += text[++i] ?? "";
      else if (c === '"') inString = false;
    } else if (c === '"') {
      inString = true;
      out += c;
    } else if (!" \t\n\r".includes(c)) {
      out += c;
    }
  }
  return out;
}

/** A Toha-produced document, as compact text. */
export function document(name: string): string {
  return compactJson(readFileSync(new URL(name, documentsDir), "utf8"));
}

/**
 * A staged session as opaque JSON text. It holds values that JSON.parse would
 * change on the way back (a 20-digit integer, 1.0, -0.0, an escaped solidus),
 * so only a byte-for-byte return matches.
 */
function sessionText(job: FixtureJob): string {
  return `{"target":"/srv/workspaces/${job.workspace}","template":"https://example.test/templates/sample-kit@v1","now":"2026-01-01T00:00:00Z","counter":12345678901234567890,"ratio":1.0,"offset":-0.0,"path":"a\\/b","step":${job.step},"submissions":[]}`;
}

export const summary = (job: FixtureJob) => ({
  jobId: job.jobId,
  workspace: job.workspace,
  operation: job.operation,
  state: job.state,
  phase: job.phase,
  createdAt: job.createdAt,
  finishedAt: job.finishedAt,
  lastEventId: job.events.length,
  error: job.error,
  deadline: null,
  limit: null,
});

export function send(res: ServerResponse, event: StoredEvent) {
  res.write(`id: ${event.id}\nevent: ${event.type}\n`);
  for (const line of event.data.split("\n")) res.write(`data: ${line}\n`);
  res.write("\n");
}

export function emit(job: FixtureJob, type: string, data: string) {
  const event = { id: job.events.length + 1, type, data };
  job.events.push(event);
  for (const res of job.listeners) {
    send(res, event);
    if (type === "result") res.end();
  }
  if (type === "result") job.listeners.clear();
}

export function setState(job: FixtureJob, state: string, phase: string) {
  job.state = state;
  job.phase = phase;
  emit(job, "state", JSON.stringify({ state, phase, deadline: null, limit: null }));
}

export const log = (job: FixtureJob, text: string) => emit(job, "log", JSON.stringify({ text }));

export function finish(
  job: FixtureJob,
  state: "succeeded" | "failed" | "ended",
  error: FixtureJob["error"] = null,
  apply?: unknown,
) {
  job.state = state;
  job.error = error;
  job.finishedAt = new Date().toISOString();
  const result: Record<string, unknown> = { state, error };
  if (apply !== undefined) result.apply = apply;
  emit(job, "result", JSON.stringify(result));
}

/** Sends an InterviewReply, built as text so the session is exactly what was sent. */
export function interview(job: FixtureJob, state: string, doc: string | null): string {
  job.step += 1;
  const session = sessionText(job);
  job.sentSession = session;
  const reply = `{"session":${session},"state":${JSON.stringify(state)},"document":${doc ?? "null"},"warnings":[]}`;
  emit(job, "interview", reply);
  return reply;
}

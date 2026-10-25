import { parseInterviewReply } from "../api/client.ts";
import {
  terminalStates,
  type InterviewReply,
  type Job,
  type JobEvent,
  type JobResult,
} from "../api/types.ts";

export type Connection = "connecting" | "open" | "closed" | "lost" | "gone";

/** What the browser knows about one job, folded from its summary and its event stream. */
export interface JobView {
  job: Job | null;
  state: string;
  phase: string;
  phases: string[];
  logs: { id: number; text: string }[];
  interview: InterviewReply | null;
  /** Question batches seen; a rejected batch coming back does not count again. */
  batch: number;
  result: JobResult | null;
  lastEventId: number;
  connection: Connection;
}

export type JobAction =
  | { type: "summary"; job: Job }
  | { type: "event"; event: JobEvent }
  | { type: "interview"; reply: InterviewReply }
  | { type: "connection"; connection: Connection };

export const initialJobView: JobView = {
  job: null,
  state: "",
  phase: "",
  phases: [],
  logs: [],
  interview: null,
  batch: 0,
  result: null,
  lastEventId: 0,
  connection: "connecting",
};

export function isTerminal(view: JobView): boolean {
  return view.result !== null || terminalStates.has(view.state);
}

function withPhase(view: JobView, phase: string): JobView {
  if (phase === "" || view.phases.includes(phase)) return { ...view, phase: phase || view.phase };
  return { ...view, phase, phases: [...view.phases, phase] };
}

function batchKey(reply: InterviewReply | null): string | null {
  const doc = reply?.document;
  if (!reply || reply.state !== "questions" || doc?.status !== "questions") return null;
  return JSON.stringify(Object.keys(doc.schema.properties ?? {}));
}

function withReply(view: JobView, reply: InterviewReply): JobView {
  if (sameReply(view.interview, reply)) return view;
  const key = batchKey(reply);
  const rejection =
    key !== null &&
    reply.document?.status === "questions" &&
    reply.document.errors !== undefined &&
    key === batchKey(view.interview);
  const batch = key !== null && !rejection ? view.batch + 1 : view.batch;
  return { ...view, interview: reply, batch };
}

function sameReply(a: InterviewReply | null, b: InterviewReply): boolean {
  return (
    a !== null &&
    a.session === b.session &&
    a.state === b.state &&
    a.documentText === b.documentText
  );
}

export function jobReducer(view: JobView, action: JobAction): JobView {
  switch (action.type) {
    case "summary": {
      const next = { ...view, job: action.job };
      // The stream is the fresher source once it has delivered an event.
      if (view.lastEventId > 0) return next;
      // Phases come from the stream in order; the summary only names the current one.
      return { ...next, state: action.job.state, phase: action.job.phase };
    }
    case "interview":
      return withReply(view, action.reply);
    case "connection":
      return { ...view, connection: action.connection };
    case "event": {
      const { event } = action;
      // A reconnect replays after Last-Event-ID; never apply an event twice.
      if (event.id <= view.lastEventId) return view;
      const seen = { ...view, lastEventId: event.id };
      switch (event.type) {
        case "state":
          return withPhase({ ...seen, state: event.data.state }, event.data.phase);
        case "log":
          return { ...seen, logs: [...seen.logs, { id: event.id, text: event.data.text }] };
        case "interview":
          return withReply(seen, event.data);
        case "result":
          return { ...seen, state: event.data.state, result: event.data };
        default: {
          const text = `[${event.event}] ${typeof event.data === "string" ? event.data : JSON.stringify(event.data)}`;
          return { ...seen, logs: [...seen.logs, { id: event.id, text }] };
        }
      }
    }
  }
}

/** Decodes one SSE message into a JobEvent; the interview session stays raw. */
export function decodeEvent(type: string, id: string, data: string): JobEvent {
  const n = Number.parseInt(id, 10);
  const eventId = Number.isFinite(n) ? n : 0;
  try {
    return decodeKnown(type, eventId, data);
  } catch {
    // A payload that does not decode is shown as text rather than lost.
    return { id: eventId, type: "unknown", event: type, data };
  }
}

function decodeKnown(type: string, eventId: number, data: string): JobEvent {
  switch (type) {
    case "state":
    case "log":
    case "result":
      return { id: eventId, type, data: JSON.parse(data) } as JobEvent;
    case "interview":
      return { id: eventId, type, data: parseInterviewReply(data) };
    default: {
      let parsed: unknown = data;
      try {
        parsed = JSON.parse(data);
      } catch {
        // Unknown events are shown as text.
      }
      return { id: eventId, type: "unknown", event: type, data: parsed };
    }
  }
}

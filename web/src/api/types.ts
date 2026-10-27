// Browser DTOs of the workspace service. web/docs/browser-api.md is the
// contract; the runtime's docs/specifications/workspace-api.yml defines the
// workspace and job members these mirror.
import type { RawJson } from "./rawJson.ts";

export type WorkspaceState = "running" | "stopped" | "no-container";

export interface ActiveJob {
  jobId: string;
  operation: string;
  state: string;
}

export interface Workspace {
  name: string;
  state: WorkspaceState | (string & {});
  containerId: string | null;
  activeJob: ActiveJob | null;
}

export interface Template {
  id: string;
  name: string;
}

export type Operation = "create" | "upgrade" | "start" | "stop" | "rebuild";

export interface ErrorBody {
  kind: string;
  message: string;
  activeJobId?: string;
}

export type JobState = "running" | "waiting-for-answers" | "succeeded" | "failed" | "ended";

export interface Job {
  jobId: string;
  workspace: string;
  operation: Operation | (string & {});
  state: JobState | (string & {});
  phase: string;
  createdAt: string;
  finishedAt: string | null;
  lastEventId: number;
  error: ErrorBody | null;
  /** When `limit` passes; null without a limit, while waiting for answers and once ended. */
  deadline: string | null;
  limit: JobLimit | null;
}

/** The time limit that ends first; limits count running time only. */
export interface JobLimit {
  seconds: number;
  scope: "job" | "phase" | (string & {});
}

export const terminalStates: ReadonlySet<string> = new Set(["succeeded", "failed", "ended"]);

// ---- Toha interview documents (interview-protocol, protocol 1) ----

export interface InterviewContext {
  target: string;
  template: string;
  commit: string | null;
}

export interface JsonSchemaObject {
  type: "object";
  properties: Record<string, PropertySchema>;
  required?: string[];
  [key: string]: unknown;
}

export interface PropertySchema {
  title?: string;
  description?: string;
  default?: unknown;
  examples?: unknown[];
  type?: string | string[];
  enum?: unknown[];
  anyOf?: PropertySchema[];
  items?: PropertySchema;
  minLength?: number;
  maxLength?: number;
  pattern?: string;
  minItems?: number;
  maxItems?: number;
  uniqueItems?: boolean;
  "x-toha-type"?: string;
  "x-toha-multiline"?: boolean;
  "x-toha-schemas"?: string[];
  "x-toha-input"?: string;
  [key: string]: unknown;
}

export interface BatchDocument {
  protocol: 1;
  status: "questions";
  context: InterviewContext;
  schema: JsonSchemaObject;
  messages: string[];
  errors?: Record<string, string[]>;
}

export interface EndedDocument {
  protocol: 1;
  status: "ended";
  context: InterviewContext;
  kind: "stop" | "abort" | (string & {});
  label?: string;
  messages: string[];
}

export interface InterviewReply {
  /** The staged session, carried back to the server byte for byte. */
  session: RawJson;
  state: "questions" | "completed" | "ended" | (string & {});
  document: BatchDocument | EndedDocument | null;
  /** The document's text as the service sent it, so defaults keep every digit. */
  documentText: string | null;
  warnings: string[];
}

export interface Submission {
  template: string;
  answers: Record<string, unknown>;
}

// ---- Job results ----

export interface ApplyReply {
  outcome: "written" | "already-current" | "needs-trust" | "planned" | "conflicted" | (string & {});
  files?: { path: string; action: string }[];
  recordPath?: string;
  messages?: string[];
  hooks?: { id: string | null; exitCode: number | null; success: boolean }[];
  conflicts?: string[];
}

export interface JobResult {
  state: string;
  error: ErrorBody | null;
  apply?: ApplyReply;
}

export type JobEvent =
  | {
      id: number;
      type: "state";
      data: { state: string; phase: string; deadline: string | null; limit: JobLimit | null };
    }
  | { id: number; type: "log"; data: { text: string } }
  | { id: number; type: "interview"; data: InterviewReply }
  | { id: number; type: "result"; data: JobResult }
  | { id: number; type: "unknown"; event: string; data: unknown };

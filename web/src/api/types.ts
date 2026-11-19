// Browser DTOs of the workspace service. docs/specifications/workspace-api.yml is the
// contract; the runtime's docs/specifications/workspace-api.yml defines the
// workspace and job members these mirror.
import type { RawJson } from "./rawJson.ts";

export type WorkspaceState = "running" | "stopped" | "no-container" | "no-folder";

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
  policies: string[];
  needsChoice: string[];
  selectionRevision: number;
  appliedRevision: number | null;
  applyFailure: { kind: string; message: string } | null;
  selectionImported: boolean;
  xolotlKey: string | null;
  /** The workspace's registered template, or null when its source is not registered. */
  template: TemplateRef | null;
  /** The template version last applied, or null while it is unknown. */
  appliedVersion: string | null;
  /** The commit last applied, or null when none is known. */
  appliedCommit: string | null;
  /** The template's current version is newer than appliedVersion. */
  needsUpgrade: boolean;
}

export interface TemplateRef {
  id: string;
  name: string;
}

/** A registered template. */
export interface Template {
  id: string;
  name: string;
  /** The address without a ref. */
  address: string;
  currentVersion: string;
  runnerImage: string | null;
  /** The names of the workspaces that use it. */
  workspaces: string[];
  /** The newest found version, or null when none is found. */
  newestVersion: string | null;
  /** The found versions, in ascending order. */
  versions: TemplateVersion[];
  /** A check found the current version's tag at another commit than the recorded one. */
  currentMoved: boolean;
  /** The last version check, or null before the first. */
  lastCheck: { at: string; failure: CheckFailure | (string & {}) | null } | null;
}

/** A found version of a template: a tag of its repository and its commit. */
export interface TemplateVersion {
  version: string;
  tag: string;
  commit: string;
}

/** Why a version check failed. */
export type CheckFailure =
  | "template-not-found"
  | "template-resolve-failed"
  | "invalid-address"
  | "template-service-unavailable";

/** The registration checks, in the order they run. */
export type TemplateCheck = "address" | "fetch" | "version" | "read" | "definition";

export type Operation =
  | "create"
  | "upgrade"
  | "start"
  | "stop"
  | "rebuild"
  | "commit"
  | "apply-branch"
  | "delete"
  | "apply-policies";

export interface ErrorBody {
  kind: string;
  message: string;
  activeJobId?: string;
  /** With template-check-failed: the first registration check that failed. */
  check?: TemplateCheck | (string & {});
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

/** A one-time T3 Code pairing token. The service keeps no copy. */
export interface T3CodePairingToken {
  token: string;
  expiresAt: string;
}

export interface PolicyGroup {
  id?: number;
  name: string;
  mode: "single" | "multi";
  policies: string[];
}
export interface PolicySettings {
  required: string[];
  groups: PolicyGroup[];
  revision: number;
}
export interface Policies {
  grant: { agents: string[]; policies: string[]; actions: string[] } | null;
  policies: { name: string; valueNames: string[]; inGrant: boolean }[];
  settings: PolicySettings;
}

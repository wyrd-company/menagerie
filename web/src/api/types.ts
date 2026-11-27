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
  /** The registered backup repository the workspace backs up to, or null when it has none. */
  backupRepository: BackupRepositoryRef | null;
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

export interface BackupRepositoryRef {
  id: string;
  name: string;
  url: string;
}

/**
 * What a backup repository's transfers authenticate with: a GitHub App by
 * its configured name, or an SSH key by its public key, with its
 * fingerprint.
 */
export type BackupCredential = { githubApp: string } | { sshKey: string; fingerprint: string };

/** A credential a registration or a change names. */
export type BackupCredentialChoice = { githubApp: string } | { sshKey: string };

/** A registered backup repository, with the names of the workspaces that back up to it. */
export interface BackupRepository extends BackupRepositoryRef {
  /**
   * Null for a registration an earlier release made, whose URL chooses
   * its credential.
   */
  credential: BackupCredential | null;
  workspaces: string[];
}

/** A backup repository's registration checks, in the order they run. */
export type BackupCheck = "url" | "credential" | "write";

/** A configured GitHub App: it serves https URLs `{server}/{owner}/{repository}`. */
export interface GitHubAppChoice {
  name: string;
  server: string;
}

/** An SSH key Wyrwood's agent offers, named by its Transit key. */
export interface SSHKeyChoice {
  name: string;
  fingerprint: string;
  /** Authorized-keys form with no comment: the line a Git host takes. */
  publicKey: string;
}

/** The credentials a backup repository can name. */
export interface Credentials {
  githubApps: GitHubAppChoice[];
  sshKeys: SSHKeyChoice[];
  /** Null when the agent listed its keys, else why it listed none. */
  sshAgent: string | null;
}

/** A registered template. */
export interface Template {
  id: string;
  name: string;
  /** The address without a ref. */
  address: string;
  currentVersion: string;
  runnerImage: string | null;
  /**
   * What every Git operation on its repository authenticates with; null
   * for a template an earlier release registered or imported, whose URL
   * chooses its credential.
   */
  credential: BackupCredential | null;
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
export type TemplateCheck = "address" | "credential" | "fetch" | "version" | "read" | "definition";

export type Operation =
  | "create"
  | "upgrade"
  | "start"
  | "stop"
  | "rebuild"
  | "commit"
  | "apply-branch"
  | "delete"
  | "apply-policies"
  | "move-backup";

export interface ErrorBody {
  kind: string;
  message: string;
  activeJobId?: string;
  /** With template-check-failed and backup-repository-check-failed: the first registration check that failed. */
  check?: TemplateCheck | BackupCheck | (string & {});
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

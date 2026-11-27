import { objectFromRaw, stringifyJson, topLevelMembers, type RawJson } from "./rawJson.ts";
import type {
  BackupCredentialChoice,
  BackupRepository,
  Credentials,
  ErrorBody,
  InterviewReply,
  Job,
  Operation,
  Policies,
  PolicySettings,
  Submission,
  T3CodePairingToken,
  Template,
  Workspace,
} from "./types.ts";

/** A refused or failed request, with the service's error body when it sent one. */
export class ApiError extends Error {
  readonly status: number;
  readonly body: ErrorBody | null;

  constructor(status: number, body: ErrorBody | null, fallback: string) {
    super(body?.message ?? fallback);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }

  get kind(): string {
    return this.body?.kind ?? `http-${this.status}`;
  }
}

let redirecting = false;

/**
 * Sends the browser to the OIDC login, returning to the current page afterwards.
 * One lost session starts one navigation: a later one would cancel the first.
 */
function redirectToLogin(): void {
  if (redirecting) return;
  redirecting = true;
  allowNextRedirect();
  const back = window.location.pathname + window.location.search;
  window.location.assign(`/auth/login?return=${encodeURIComponent(back)}`);
}

/**
 * Ends the guard when the page outlives its navigation: the person's next action
 * after they cancelled it, or a restore from the back/forward cache.
 */
function allowNextRedirect(): void {
  const allow = () => {
    redirecting = false;
    window.removeEventListener("pointerdown", allow, true);
    window.removeEventListener("keydown", allow, true);
    window.removeEventListener("pageshow", onShow);
  };
  const onShow = (event: PageTransitionEvent) => {
    if (event.persisted) allow();
  };
  window.addEventListener("pointerdown", allow, true);
  window.addEventListener("keydown", allow, true);
  window.addEventListener("pageshow", onShow);
}

async function request(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body !== undefined) headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...init, headers, credentials: "same-origin" });
  if (response.status === 401) {
    redirectToLogin();
    throw new ApiError(401, { kind: "unauthenticated", message: "Signing in…" }, "Signing in…");
  }
  if (!response.ok) throw await errorFrom(response);
  return response;
}

async function errorFrom(response: Response): Promise<ApiError> {
  const fallback = `${response.status} ${response.statusText}`.trim();
  try {
    const parsed = (await response.json()) as { error?: ErrorBody };
    return new ApiError(response.status, parsed.error ?? null, fallback);
  } catch {
    return new ApiError(response.status, null, fallback);
  }
}

async function json<T>(path: string, init?: RequestInit): Promise<T> {
  return (await (await request(path, init)).json()) as T;
}

export async function listWorkspaces(): Promise<Workspace[]> {
  return (await json<{ workspaces: Workspace[] }>("/api/workspaces")).workspaces;
}

export async function listTemplates(): Promise<Template[]> {
  return (await json<{ templates: Template[] }>("/api/templates")).templates;
}

export interface TemplateRegistration {
  name: string;
  address: string;
  runnerImage?: string;
  /** As a backup repository's, for the Git URL the address names. */
  credential?: BackupCredentialChoice | null;
  /** Run the checks alone and store nothing. */
  check?: boolean;
}

/** Registers a template once it passes the minimum checks; with check, runs the checks alone. */
export const registerTemplate = (registration: TemplateRegistration) =>
  json<Template>("/api/templates", { method: "POST", body: JSON.stringify(registration) });

/** The Git URL a template address names, which its credential has to fit; it reaches no network. */
export const readTemplateAddress = async (address: string) =>
  (
    await json<{ url: string }>("/api/templates/addresses", {
      method: "POST",
      body: JSON.stringify({ address }),
    })
  ).url;

/** Changes a template's credential once its checks pass; with check, runs them alone. */
export const setTemplateCredential = (
  id: string,
  credential: BackupCredentialChoice,
  check?: boolean,
) =>
  json<Template>(`/api/templates/${encodeURIComponent(id)}/credential`, {
    method: "PUT",
    body: JSON.stringify({ credential, ...(check ? { check } : {}) }),
  });

/** Makes a newer version of the template's repository its current version; with check, runs its checks alone. */
export const upgradeTemplate = (id: string, version: string, check = false) =>
  json<Template>(`/api/templates/${encodeURIComponent(id)}/upgrades`, {
    method: "POST",
    body: JSON.stringify(check ? { version, check } : { version }),
  });

/** Checks a template for new versions now; the template answers with the check's record. */
export const checkTemplateVersions = (id: string) =>
  json<Template>(`/api/templates/${encodeURIComponent(id)}/version-checks`, { method: "POST" });

/** Renames a template or changes its runner image; null runs its hooks in the default. */
export const editTemplate = (id: string, edit: { name?: string; runnerImage?: string | null }) =>
  json<Template>(`/api/templates/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify(edit),
  });

/** Removes a template no workspace uses. */
export async function removeTemplate(id: string): Promise<void> {
  await request(`/api/templates/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function listBackupRepositories(): Promise<BackupRepository[]> {
  return (await json<{ backupRepositories: BackupRepository[] }>("/api/backup-repositories"))
    .backupRepositories;
}

export interface BackupRepositoryRegistration {
  name: string;
  url: string;
  /** Required for an SSH URL and for an https URL an App's server holds. */
  credential?: BackupCredentialChoice | null;
  /** Run the checks alone and store nothing. */
  check?: boolean;
}

/** Registers a backup repository once Menagerie can write to it; with check, runs the checks alone. */
export const registerBackupRepository = (registration: BackupRepositoryRegistration) =>
  json<BackupRepository>("/api/backup-repositories", {
    method: "POST",
    body: JSON.stringify(registration),
  });

/** Changes a backup repository's credential once its checks pass; with check, runs them alone. */
export const setBackupRepositoryCredential = (
  id: string,
  credential: BackupCredentialChoice,
  check?: boolean,
) =>
  json<BackupRepository>(`/api/backup-repositories/${encodeURIComponent(id)}/credential`, {
    method: "PUT",
    body: JSON.stringify({ credential, ...(check ? { check } : {}) }),
  });

/** The credentials a backup repository or a template can name. */
export const listCredentials = () => json<Credentials>("/api/credentials");

/** Removes a backup repository no workspace backs up to; the repository itself is left as it is. */
export async function removeBackupRepository(id: string): Promise<void> {
  await request(`/api/backup-repositories/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function mintT3CodePairingToken(workspace: string): Promise<T3CodePairingToken> {
  return json<T3CodePairingToken>(
    `/api/workspaces/${encodeURIComponent(workspace)}/t3-code-pairing-tokens`,
    { method: "POST" },
  );
}

export async function listJobs(): Promise<Job[]> {
  return (await json<{ jobs: Job[] }>("/api/jobs")).jobs;
}

export async function getJob(jobId: string): Promise<Job> {
  return json<Job>(`/api/jobs/${encodeURIComponent(jobId)}`);
}

export interface StartJob {
  policies?: string[];
  operation: Operation;
  workspace: string;
  templateId?: string;
  /** The registered backup repository a create backs the workspace up to, or a move-backup moves its backups to. */
  backupRepositoryId?: string;
  /** The branch of the workspace repository an apply-branch job applies. */
  branch?: string;
  /** An upgrade asks every question again, with the recorded answers as defaults. */
  reanswer?: boolean;
}

export async function startJob(start: StartJob): Promise<Job> {
  return json<Job>("/api/jobs", { method: "POST", body: JSON.stringify(start) });
}

/** Parses an InterviewReply, keeping `session` as the raw text the server sent. */
export function parseInterviewReply(text: string): InterviewReply {
  const members = topLevelMembers(text);
  const session = members.get("session");
  if (session === undefined) throw new SyntaxError("interview reply has no session");
  const parse = (name: string): unknown => {
    const raw = members.get(name);
    return raw === undefined ? undefined : JSON.parse(raw);
  };
  return {
    session: session as RawJson,
    state: parse("state") as InterviewReply["state"],
    document: (parse("document") ?? null) as InterviewReply["document"],
    documentText: members.get("document") === "null" ? null : (members.get("document") ?? null),
    warnings: (parse("warnings") as string[] | undefined) ?? [],
  };
}

/** Answers the job's current interview batch with the full session it came with. */
export async function answerJob(
  jobId: string,
  session: RawJson,
  submission: Submission,
): Promise<InterviewReply | null> {
  const body = objectFromRaw([
    ["session", session],
    // Typed JSON answers go out as typed, so their numbers keep every digit.
    ["submission", stringifyJson(submission)],
  ]);
  const response = await request(`/api/jobs/${encodeURIComponent(jobId)}/answers`, {
    method: "POST",
    body,
  });
  const text = await response.text();
  return text.trim() === "" ? null : parseInterviewReply(text);
}

export const listPolicies = () => json<Policies>("/api/policies");
export const savePolicySettings = (settings: PolicySettings) =>
  json<PolicySettings>("/api/policies/settings", { method: "PUT", body: JSON.stringify(settings) });

export const saveWorkspacePolicies = (name: string, policies: string[], revision: number) =>
  json<{ selectionRevision: number; jobId: string | null }>(
    `/api/workspaces/${encodeURIComponent(name)}/policies`,
    { method: "PUT", body: JSON.stringify({ policies, revision }) },
  );

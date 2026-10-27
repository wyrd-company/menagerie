import { objectFromRaw, stringifyJson, topLevelMembers, type RawJson } from "./rawJson.ts";
import type {
  ErrorBody,
  InterviewReply,
  Job,
  Operation,
  Submission,
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

/** Sends the browser to the OIDC login, returning to the current page afterwards. */
export function redirectToLogin(location: Location = window.location): void {
  const back = location.pathname + location.search;
  location.assign(`/auth/login?return=${encodeURIComponent(back)}`);
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

export async function listJobs(): Promise<Job[]> {
  return (await json<{ jobs: Job[] }>("/api/jobs")).jobs;
}

export async function getJob(jobId: string): Promise<Job> {
  return json<Job>(`/api/jobs/${encodeURIComponent(jobId)}`);
}

export interface StartJob {
  operation: Operation;
  workspace: string;
  templateId?: string;
  /** The branch of the workspace repository an apply-branch job applies. */
  branch?: string;
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

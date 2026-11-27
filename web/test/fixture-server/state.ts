// Fixture state: workspaces, jobs and the options browser tests can set.
import { createHash } from "node:crypto";
import type { ServerResponse } from "node:http";

export interface FixtureWorkspace {
  name: string;
  state: "running" | "stopped" | "no-container" | "no-folder";
  containerId: string | null;
  /** The registered template's id, or null. */
  template: string | null;
  /** The version last applied, which an upgrade must be older than; null while unknown. */
  applied: string | null;
  /** The commit last applied, or null. */
  appliedCommit?: string | null;
  /** The registered backup repository's id, or null; absent, the first registered one. */
  backups?: string | null;
}

/** A backup repository's or template's credential: an App by name, or an SSH key by its public key. */
export type FixtureCredential = { githubApp: string } | { sshKey: string } | null;

/** A registered backup repository. */
export interface FixtureBackupRepository {
  id: string;
  name: string;
  url: string;
  /** Null for a registration an earlier release made. */
  credential: FixtureCredential;
}

/** The fixture's credentials: one App on git.example.test, and the agent's one Git key. */
export const fixtureCredentials = {
  githubApps: [{ name: "example-org", server: "https://git.example.test" }],
  sshKeys: [
    {
      name: "menagerie-ssh",
      fingerprint: "SHA256:fixtureMenagerieSshKeyFingerprint0000000000",
      publicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureMenagerieSshPublicKey000000000000",
    },
  ],
  sshAgent: null as string | null,
};

/** A registered template, the versions its repository has and those its checks found. */
export interface FixtureTemplate {
  id: string;
  name: string;
  address: string;
  currentVersion: string;
  runnerImage: string | null;
  /** Its credential; absent or null, one an earlier release registered, chosen by URL. */
  credential?: FixtureCredential;
  /** The versions its repository has: what a version check finds. */
  versions: string[];
  /** The versions its last successful check found; absent, versions. */
  found?: string[];
  /** When its last check ended, and why it failed; absent, a check an hour ago succeeded. */
  lastCheck?: { at: string; failure: string | null } | null;
  /** Its repository's current tag is at another commit than the recorded one. */
  tagMoved?: boolean;
  /** A check found the current version's tag moved. */
  currentMoved?: boolean;
}

/** The fixture's commit of a template's version; a moved tag's is another. */
export const commitOf = (template: string, version: string, moved = false) =>
  createHash("sha1")
    .update(`${template} ${version}${moved ? " moved" : ""}`)
    .digest("hex");

export interface StoredEvent {
  id: number;
  type: string;
  data: string;
}

export interface FixtureJob {
  jobId: string;
  workspace: string;
  operation: string;
  templateId?: string;
  /** The backup repository a create backs up to. */
  backupRepositoryId?: string;
  /** The branch an apply-branch job applies. */
  branch?: string;
  state: string;
  phase: string;
  createdAt: string;
  finishedAt: string | null;
  error: { kind: string; message: string } | null;
  events: StoredEvent[];
  listeners: Set<ServerResponse>;
  /** The exact session text the job last sent; answers must carry it back unchanged. */
  sentSession: string | null;
  step: number;
  release: (() => void) | null;
  answerCount: number;
  lastSubmission?: string;
  stage: "first" | "second";
}

export interface FixtureOptions {
  /** "raw": the producer's whole upgrade batch; "uncovered": the narrowed synthetic one. */
  upgradeDocument: "raw" | "uncovered";
  /** Whether running workspaces have the t3code-server Feature. */
  t3CodeInstalled: boolean;
}

/** What the job scripts read and change. */
export interface World {
  workspaces: FixtureWorkspace[];
  templates: FixtureTemplate[];
  backupRepositories: FixtureBackupRepository[];
  options: FixtureOptions;
}

export const containerId = (seed: string) =>
  Array.from(
    { length: 64 },
    (_, i) => "0123456789abcdef"[(seed.charCodeAt(i % seed.length) + i) % 16],
  ).join("");

export function initialWorkspaces(): FixtureWorkspace[] {
  const kit = {
    template: "sample-kit",
    applied: "1.0.0",
    appliedCommit: commitOf("sample-kit", "1.0.0"),
  };
  return [
    { name: "sample-alpha", state: "running", containerId: containerId("alpha"), ...kit },
    { name: "sample-beta", state: "stopped", containerId: containerId("beta"), ...kit },
    { name: "sample-broken", state: "stopped", containerId: containerId("broken"), ...kit },
    { name: "sample-gamma", state: "no-container", containerId: null, ...kit },
    // Its folder is gone, and Menagerie's data still holds its binding and key.
    {
      name: "sample-gone",
      state: "no-folder",
      containerId: null,
      template: null,
      applied: null,
      backups: null,
    },
    { name: "sample-slow", state: "running", containerId: containerId("slow"), ...kit },
  ];
}

/** The registered backup repositories: one, which every workspace backs up to. */
export function initialBackupRepositories(): FixtureBackupRepository[] {
  return [
    {
      id: "example-backups",
      name: "Example backups",
      url: "ssh://git@git.example.test/example-org/workspace-backups.git",
      credential: null,
    },
  ];
}

/** The registered templates: the sample kit has a version newer than its current one. */
export function initialTemplates(): FixtureTemplate[] {
  return [
    {
      id: "sample-kit",
      name: "Sample kit",
      address: "https://git.example.test/example-org/sample-kit",
      currentVersion: "1.1.0",
      runnerImage: null,
      versions: ["1.0.0", "1.1.0", "1.2.0"],
    },
    {
      id: "tools-kit",
      name: "Tools kit",
      address: "gh:example-org/tools-kit",
      currentVersion: "2.0.0",
      runnerImage: "alpine:3.22",
      versions: ["2.0.0"],
    },
  ];
}

/** Whether version a is newer than version b, by their numbers. */
export function newer(a: string, b: string): boolean {
  const parse = (v: string) => v.replace(/^v/, "").split(".").map(Number);
  const [x, y] = [parse(a), parse(b)];
  for (let i = 0; i < 3; i++) if ((x[i] ?? 0) !== (y[i] ?? 0)) return (x[i] ?? 0) > (y[i] ?? 0);
  return false;
}

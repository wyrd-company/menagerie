// Fixture state: workspaces, jobs and the options browser tests can set.
import type { ServerResponse } from "node:http";

export interface FixtureWorkspace {
  name: string;
  state: "running" | "stopped" | "no-container" | "no-folder";
  containerId: string | null;
  /** The registered template's id, or null. */
  template: string | null;
  /** The version last applied, which an upgrade must be older than. */
  applied: string | null;
}

/** A registered template, and the versions its repository has. */
export interface FixtureTemplate {
  id: string;
  name: string;
  address: string;
  currentVersion: string;
  runnerImage: string | null;
  versions: string[];
}

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
  options: FixtureOptions;
}

export const containerId = (seed: string) =>
  Array.from(
    { length: 64 },
    (_, i) => "0123456789abcdef"[(seed.charCodeAt(i % seed.length) + i) % 16],
  ).join("");

export function initialWorkspaces(): FixtureWorkspace[] {
  const kit = { template: "sample-kit", applied: "1.0.0" };
  return [
    { name: "sample-alpha", state: "running", containerId: containerId("alpha"), ...kit },
    { name: "sample-beta", state: "stopped", containerId: containerId("beta"), ...kit },
    { name: "sample-broken", state: "stopped", containerId: containerId("broken"), ...kit },
    { name: "sample-gamma", state: "no-container", containerId: null, ...kit },
    // Its folder is gone, and Menagerie's data still holds its binding and key.
    { name: "sample-gone", state: "no-folder", containerId: null, template: null, applied: null },
    { name: "sample-slow", state: "running", containerId: containerId("slow"), ...kit },
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

// Fixture state: workspaces, jobs and the options browser tests can set.
import type { ServerResponse } from "node:http";

export interface FixtureWorkspace {
  name: string;
  state: "running" | "stopped" | "no-container";
  containerId: string | null;
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
}

/** What the job scripts read and change. */
export interface World {
  workspaces: FixtureWorkspace[];
  options: FixtureOptions;
}

export const containerId = (seed: string) =>
  Array.from(
    { length: 64 },
    (_, i) => "0123456789abcdef"[(seed.charCodeAt(i % seed.length) + i) % 16],
  ).join("");

export function initialWorkspaces(): FixtureWorkspace[] {
  return [
    { name: "sample-alpha", state: "running", containerId: containerId("alpha") },
    { name: "sample-beta", state: "stopped", containerId: containerId("beta") },
    { name: "sample-broken", state: "stopped", containerId: containerId("broken") },
    { name: "sample-gamma", state: "no-container", containerId: null },
    { name: "sample-slow", state: "running", containerId: containerId("slow") },
  ];
}

export const templates = [
  { id: "sample-kit-v1", name: "Sample kit, version 1" },
  { id: "sample-kit-v2", name: "Sample kit, version 2" },
];

// The upgrade form starts an upgrade job, and asks for every question again
// only when the manager says so.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { UpgradeWorkspace } from "./UpgradeWorkspace.tsx";

const job = {
  jobId: "j1",
  workspace: "sample",
  operation: "upgrade",
  state: "running",
  phase: "backup-local-changes",
  createdAt: "2026-01-01T00:00:00Z",
  finishedAt: null,
  lastEventId: 0,
  error: null,
  deadline: null,
  limit: null,
};

/** Stubs the API and returns the bodies posted to /api/jobs. */
function stubApi(): unknown[] {
  const started: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      if (path === "/api/workspaces") {
        return Response.json({
          workspaces: [{ name: "sample", state: "running", containerId: "c1", activeJob: null }],
        });
      }
      if (path === "/api/templates") {
        return Response.json({ templates: [{ id: "sample-v2", name: "Sample 2" }] });
      }
      if (path === "/api/jobs" && init?.method === "POST") {
        started.push(JSON.parse(String(init.body)));
        return Response.json(job, { status: 202 });
      }
      return Response.json({ kind: "not-found", message: "not stubbed" }, { status: 404 });
    }),
  );
  return started;
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

describe("the upgrade form", () => {
  it("asks only what the recorded answers do not settle by default", async () => {
    const started = stubApi();
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    await userEvent.click(await screen.findByRole("radio", { name: /Sample 2/ }));
    await userEvent.click(screen.getByRole("button", { name: "Begin upgrade" }));
    await waitFor(() => expect(started).toHaveLength(1));
    expect(started[0]).toEqual({
      operation: "upgrade",
      workspace: "sample",
      templateId: "sample-v2",
    });
  });

  it("asks every question again when the box is checked", async () => {
    const started = stubApi();
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    await userEvent.click(await screen.findByRole("radio", { name: /Sample 2/ }));
    const box = screen.getByRole("checkbox", { name: "Ask every question again" });
    expect(box).not.toBeChecked();
    await userEvent.click(box);
    await userEvent.click(screen.getByRole("button", { name: "Begin upgrade" }));
    await waitFor(() => expect(started).toHaveLength(1));
    expect(started[0]).toEqual({
      operation: "upgrade",
      workspace: "sample",
      templateId: "sample-v2",
      reanswer: true,
    });
  });
});

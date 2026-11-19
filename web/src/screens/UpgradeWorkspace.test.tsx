// The upgrade form starts an upgrade of the workspace's own template to its
// current version, and asks for every question again only when the manager
// says so.
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Template, Workspace } from "../api/types.ts";
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
function stubApi(
  template: Workspace["template"] = { id: "sample", name: "Sample kit" },
  applied: Pick<Workspace, "appliedVersion" | "appliedCommit" | "needsUpgrade"> = {
    appliedVersion: "1.0.0",
    appliedCommit: "c100",
    needsUpgrade: true,
  },
  currentMoved = false,
): unknown[] {
  const started: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      if (path === "/api/workspaces") {
        return Response.json({
          workspaces: [
            {
              name: "sample",
              state: "running",
              containerId: "c1",
              activeJob: null,
              policies: [],
              needsChoice: [],
              selectionRevision: 0,
              appliedRevision: 0,
              applyFailure: null,
              selectionImported: true,
              xolotlKey: null,
              template,
              ...applied,
            },
          ] satisfies Workspace[],
        });
      }
      if (path === "/api/templates") {
        return Response.json({
          templates: [
            {
              id: "sample",
              name: "Sample kit",
              address: "gh:example-org/kit",
              currentVersion: "1.1.0",
              runnerImage: null,
              workspaces: ["sample"],
              newestVersion: "1.1.0",
              versions: [
                { version: "1.0.0", tag: "1.0.0", commit: "c100" },
                { version: "1.1.0", tag: "1.1.0", commit: "c110" },
              ],
              currentMoved,
              lastCheck: { at: "2026-01-01T00:00:00Z", failure: null },
            },
          ] satisfies Template[],
        });
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
    expect(await screen.findByText("Sample kit 1.1.0")).toBeInTheDocument();
    expect(screen.getByText("Applied").nextElementSibling).toHaveTextContent("1.0.0");
    expect(screen.queryByRole("radio")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Begin upgrade" }));
    await waitFor(() => expect(started).toHaveLength(1));
    expect(started[0]).toEqual({ operation: "upgrade", workspace: "sample" });
  });

  it("asks every question again when the box is checked", async () => {
    const started = stubApi();
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    await screen.findByText("Sample kit 1.1.0");
    const box = screen.getByRole("checkbox", { name: "Ask every question again" });
    expect(box).not.toBeChecked();
    await userEvent.click(box);
    await userEvent.click(screen.getByRole("button", { name: "Begin upgrade" }));
    await waitFor(() => expect(started).toHaveLength(1));
    expect(started[0]).toEqual({ operation: "upgrade", workspace: "sample", reanswer: true });
  });

  it("refuses a workspace with no registered template", async () => {
    const started = stubApi(null);
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    expect(
      await screen.findByText("Register this workspace's template before upgrading."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Begin upgrade" })).toBeDisabled();
    expect(started).toHaveLength(0);
  });

  it("refuses while the current version's tag has moved", async () => {
    const started = stubApi(undefined, undefined, true);
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    expect(
      await screen.findByText(
        "The current tag moved. Validate and save the version on Templates before upgrading.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Begin upgrade" })).toBeDisabled();
    expect(started).toHaveLength(0);
  });

  it("says a workspace at the current version needs nothing", async () => {
    stubApi(undefined, { appliedVersion: "1.1.0", appliedCommit: "c110", needsUpgrade: false });
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    expect(
      await screen.findByText("This workspace already has the current version."),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Begin upgrade" })).toBeDisabled();
  });

  it("upgrades a workspace whose applied version is unknown, showing its commit on hover", async () => {
    const started = stubApi(undefined, {
      appliedVersion: null,
      appliedCommit: "c0ffee",
      needsUpgrade: false,
    });
    render(<UpgradeWorkspace workspace="sample" jobId={null} />);
    const unknown = await screen.findByText("Unknown version");
    expect(unknown).toHaveAttribute("title", "Applied commit c0ffee");
    await userEvent.click(screen.getByRole("button", { name: "Begin upgrade" }));
    await waitFor(() => expect(started).toHaveLength(1));
  });
});

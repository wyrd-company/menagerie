// A commit job starts from the schedule or a workspace's socket, not from a
// button: the list and the console still name it.
import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkspaceList } from "../screens/WorkspaceList.tsx";
import { JobConsole } from "./JobConsole.tsx";
import { initialJobView } from "./jobView.ts";

const job = {
  jobId: "j1",
  workspace: "sample",
  operation: "commit",
  state: "running",
  phase: "backup",
  createdAt: "2026-01-01T00:00:00Z",
  finishedAt: null,
  lastEventId: 1,
  error: null,
  deadline: null,
  limit: null,
};

afterEach(() => vi.unstubAllGlobals());

describe("a commit job", () => {
  it("is named in the console", () => {
    render(<JobConsole view={{ ...initialJobView, job }} onReconnect={() => {}} />);
    expect(screen.getByRole("heading", { name: /^Commit sample$/ })).toBeInTheDocument();
  });

  it("is named on its workspace's row", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (path: string) =>
        Response.json(
          path === "/api/jobs"
            ? { jobs: [job] }
            : {
                workspaces: [
                  {
                    name: "sample",
                    state: "running",
                    containerId: "c1",
                    activeJob: { jobId: "j1", operation: "commit", state: "running" },
                  },
                ],
              },
        ),
      ),
    );
    render(<WorkspaceList jobId={null} />);
    expect(await screen.findByText(/committing · running/)).toBeInTheDocument();
  });
});

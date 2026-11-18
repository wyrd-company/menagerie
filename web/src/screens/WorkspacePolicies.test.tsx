import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import type { Policies, Workspace } from "../api/types.ts";
import { CreateWorkspace } from "./CreateWorkspace.tsx";
import { WorkspacePolicies, policyStatus } from "./WorkspacePolicies.tsx";

const catalog: Policies = {
  grant: { agents: ["host"], policies: ["*"], actions: ["read", "bind", "change"] },
  policies: ["workspace", "bob-ssh", "ira-ssh"].map((name) => ({
    name,
    valueNames: [],
    inGrant: true,
  })),
  settings: {
    required: ["workspace"],
    revision: 1,
    groups: [{ id: 1, name: "Operator SSH", mode: "multi", policies: ["bob-ssh", "ira-ssh"] }],
  },
};
const workspace: Workspace = {
  name: "sample",
  state: "running",
  containerId: "c1",
  activeJob: null,
  policies: ["workspace", "bob-ssh"],
  needsChoice: [],
  selectionRevision: 7,
  appliedRevision: 6,
  selectionImported: true,
  applyFailure: null,
  xolotlKey: null,
  template: null,
};
afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

it("requires a group choice before create and submits required policies with the choice", async () => {
  const posted: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/policies") return Response.json(catalog);
      if (url === "/api/templates")
        return Response.json({ templates: [{ id: "v1", name: "Sample template" }] });
      posted.push(JSON.parse(String(init?.body)));
      return Response.json({ jobId: "j1" });
    }),
  );
  render(<CreateWorkspace jobId={null} />);
  await userEvent.click(await screen.findByRole("radio", { name: /Sample template/ }));
  await userEvent.type(screen.getByRole("textbox", { name: /Workspace name/ }), "sample");
  expect(await screen.findByRole("checkbox", { name: /workspace.*Required/ })).toBeDisabled();
  await userEvent.click(screen.getByRole("button", { name: "Begin interview" }));
  expect(screen.getByRole("alert")).toHaveTextContent("Choose at least one policy in Operator SSH");
  expect(posted).toHaveLength(0);
  await userEvent.click(screen.getByRole("checkbox", { name: "bob-ssh" }));
  await userEvent.click(screen.getByRole("button", { name: "Begin interview" }));
  await waitFor(() =>
    expect(posted).toEqual([
      {
        operation: "create",
        workspace: "sample",
        templateId: "v1",
        policies: ["workspace", "bob-ssh"],
      },
    ]),
  );
});

it("saves edits with the read revision while a workspace job is running", async () => {
  const posted: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/policies") return Response.json(catalog);
      posted.push(JSON.parse(String(init?.body)));
      return Response.json({ selectionRevision: 8, jobId: null });
    }),
  );
  const saved = vi.fn(async () => {});
  render(
    <WorkspacePolicies
      workspace={{
        ...workspace,
        activeJob: { jobId: "running", operation: "rebuild", state: "running" },
      }}
      onSaved={saved}
    />,
  );
  await userEvent.click(await screen.findByRole("checkbox", { name: "ira-ssh" }));
  await userEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() =>
    expect(posted).toEqual([{ policies: ["workspace", "bob-ssh", "ira-ssh"], revision: 7 }]),
  );
  expect(await screen.findByText("Saved. Policies apply after the running job.")).toBeVisible();
  expect(saved).toHaveBeenCalledOnce();
});

it("retains the draft after a stale save and reloads explicitly", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url === "/api/policies") return Response.json(catalog);
      if (url === "/api/workspaces")
        return Response.json({
          workspaces: [{ ...workspace, policies: ["workspace", "ira-ssh"], selectionRevision: 8 }],
        });
      return Response.json(
        { error: { kind: "selection-changed", message: "Policy selection changed." } },
        { status: 409 },
      );
    }),
  );
  render(<WorkspacePolicies workspace={workspace} onSaved={async () => {}} />);
  await userEvent.click(await screen.findByRole("checkbox", { name: "ira-ssh" }));
  await userEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Policy selection changed");
  expect(screen.getByRole("checkbox", { name: "bob-ssh" })).toBeChecked();
  await userEvent.click(screen.getByRole("button", { name: "Reload policies" }));
  await waitFor(() => expect(screen.getByRole("checkbox", { name: "bob-ssh" })).not.toBeChecked());
  expect(screen.getByRole("checkbox", { name: "ira-ssh" })).toBeChecked();
});

it("reports import, failed, pending and applied states", () => {
  expect(policyStatus({ ...workspace, selectionImported: false })).toBe("Policy import pending");
  expect(
    policyStatus({ ...workspace, applyFailure: { kind: "binding-refused", message: "refused" } }),
  ).toBe("Policies failed");
  expect(policyStatus(workspace)).toBe("Policies pending");
  expect(policyStatus({ ...workspace, appliedRevision: 7 })).toBe("Policies applied");
});

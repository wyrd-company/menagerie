// The backup repositories screen registers a repository once Menagerie can
// write to it, validating before it saves, shows a refusal by its check,
// lists the workspaces that back up to each, and removes one none does.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import type { BackupRepository } from "../api/types.ts";
import { BackupRepositoriesScreen } from "./BackupRepositories.tsx";
import { CreateWorkspace } from "./CreateWorkspace.tsx";

const used: BackupRepository = {
  id: "example-backups",
  name: "Example backups",
  url: "git@git.example.test:example-org/backups.git",
  workspaces: ["alpha"],
};
const idle: BackupRepository = {
  id: "client-backups",
  name: "Client backups",
  url: "https://git.example.test/client-org/backups.git",
  workspaces: [],
};

interface Call {
  method: string;
  path: string;
  body: unknown;
}

function stubApi(repositories: BackupRepository[]): Call[] {
  const calls: Call[] = [];
  let list = [...repositories];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = (init?.body ? JSON.parse(String(init.body)) : null) as {
        name: string;
        url: string;
        check?: boolean;
      } | null;
      calls.push({ method, path, body });
      if (path === "/api/backup-repositories" && method === "GET")
        return Response.json({ backupRepositories: list });
      if (path === "/api/backup-repositories" && method === "POST" && body) {
        if (body.url.includes("readonly"))
          return Response.json(
            {
              error: {
                kind: "backup-repository-check-failed",
                message: "Menagerie could not write to the repository",
                check: "write",
              },
            },
            { status: 422 },
          );
        const made = { id: "new-backups", name: body.name, url: body.url, workspaces: [] };
        if (body.check) return Response.json(made);
        list = [...list, made];
        return Response.json(made, { status: 201 });
      }
      if (method === "DELETE") {
        list = list.filter((r) => path !== `/api/backup-repositories/${r.id}`);
        return new Response(null, { status: 204 });
      }
      if (path === "/api/templates") return Response.json({ templates: [] });
      if (path === "/api/policies")
        return Response.json({
          settings: { required: [], groups: [], revision: 0 },
          policies: [],
          grant: true,
        });
      return Response.json(
        { error: { kind: "not-found", message: "not stubbed" } },
        { status: 404 },
      );
    }),
  );
  return calls;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

it("validates a repository before it saves it, and shows a refusal by its check", async () => {
  const calls = stubApi([used]);
  render(<BackupRepositoriesScreen />);
  expect(await screen.findByText("Backs up alpha")).toBeInTheDocument();
  await userEvent.click(screen.getAllByRole("button", { name: "Register repository" })[0]!);
  await userEvent.type(screen.getByRole("textbox", { name: "Display name" }), "New backups");
  await userEvent.type(
    screen.getByRole("textbox", { name: /Git URL/ }),
    "git@git.example.test:o/readonly.git",
  );
  const save = screen.getByRole("button", { name: "Save" });
  expect(save).toBeDisabled();
  await userEvent.click(screen.getByRole("button", { name: "Validate" }));
  expect(
    await screen.findByText("write: Menagerie could not write to the repository"),
  ).toBeInTheDocument();
  expect(save).toBeDisabled();

  const url = screen.getByRole("textbox", { name: /Git URL/ });
  await userEvent.clear(url);
  await userEvent.type(url, "git@git.example.test:o/new.git");
  await userEvent.click(screen.getByRole("button", { name: "Validate" }));
  await waitFor(() => expect(save).toBeEnabled());
  await userEvent.click(save);
  expect(await screen.findByRole("heading", { name: "New backups" })).toBeInTheDocument();
  expect(calls.filter((c) => c.method === "POST").map((c) => c.body)).toEqual([
    { name: "New backups", url: "git@git.example.test:o/readonly.git", check: true },
    { name: "New backups", url: "git@git.example.test:o/new.git", check: true },
    { name: "New backups", url: "git@git.example.test:o/new.git" },
  ]);
});

it("removes only a repository no workspace backs up to", async () => {
  const calls = stubApi([used, idle]);
  render(<BackupRepositoriesScreen />);
  const usedEntry = (await screen.findByRole("heading", { name: "Example backups" })).closest(
    "li",
  )!;
  expect(within(usedEntry).getByRole("button", { name: "Remove" })).toBeDisabled();
  const idleEntry = screen.getByRole("heading", { name: "Client backups" }).closest("li")!;
  await userEvent.click(within(idleEntry).getByRole("button", { name: "Remove" }));
  await waitFor(() =>
    expect(screen.queryByRole("heading", { name: "Client backups" })).not.toBeInTheDocument(),
  );
  expect(
    calls.some(
      (c) => c.method === "DELETE" && c.path === "/api/backup-repositories/client-backups",
    ),
  ).toBe(true);
});

it("asks for a backup repository on create when several are registered", async () => {
  stubApi([used, idle]);
  render(<CreateWorkspace jobId={null} />);
  expect(await screen.findByRole("radio", { name: /Client backups/ })).not.toBeChecked();
  expect(screen.getByRole("radio", { name: /Example backups/ })).not.toBeChecked();
  await userEvent.click(screen.getByRole("button", { name: "Begin interview" }));
  expect(await screen.findByText("Choose a backup repository.")).toBeInTheDocument();
});

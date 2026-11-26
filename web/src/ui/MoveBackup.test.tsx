// The Move backups form offers every registered backup repository but the
// workspace's own, and moves only once one is chosen.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import type { BackupRepository, Workspace } from "../api/types.ts";
import { MoveBackup } from "./MoveBackup.tsx";

const own: BackupRepository = {
  id: "example-backups",
  name: "Example backups",
  url: "git@git.example.test:example-org/backups.git",
  credential: null,
  workspaces: ["sample"],
};
const client: BackupRepository = {
  id: "client-backups",
  name: "Client backups",
  url: "https://git.example.test/client-org/backups.git",
  credential: { githubApp: "client-org" },
  workspaces: [],
};
const workspace = {
  name: "sample",
  backupRepository: { id: own.id, name: own.name, url: own.url },
} as Workspace;

afterEach(() => vi.unstubAllGlobals());

const stub = (repositories: BackupRepository[]) =>
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ backupRepositories: repositories })),
  );

it("offers the other repositories and moves to the chosen one", async () => {
  stub([own, client]);
  const onMove = vi.fn();
  render(<MoveBackup workspace={workspace} busy={false} onMove={onMove} onCancel={() => {}} />);
  const select = await screen.findByRole("combobox", { name: "Move backups to" });
  expect(screen.queryByRole("option", { name: /Example backups/ })).not.toBeInTheDocument();
  expect(screen.getByText(/Its earlier backups stay in Example backups/)).toBeInTheDocument();
  const move = screen.getByRole("button", { name: "Move backups" });
  expect(move).toBeDisabled();
  await userEvent.selectOptions(select, "client-backups");
  await userEvent.click(move);
  expect(onMove).toHaveBeenCalledWith("client-backups");
});

it("says so when no other repository is registered", async () => {
  stub([own]);
  render(<MoveBackup workspace={workspace} busy={false} onMove={() => {}} onCancel={() => {}} />);
  expect(await screen.findByText(/No other backup repository is registered/)).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Move backups" })).toBeDisabled();
});

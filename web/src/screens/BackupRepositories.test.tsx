// The backup repositories screen registers a repository with its
// credential once Menagerie can write to it, validating before it saves,
// shows a refusal by its check, lists the workspaces that back up to each
// and the credential of each, changes that credential after a Validate,
// and removes a repository none backs up to.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import type { BackupRepository, Credentials } from "../api/types.ts";
import { BackupRepositoriesScreen } from "./BackupRepositories.tsx";
import { CreateWorkspace } from "./CreateWorkspace.tsx";

const gitKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISampleGitKeySampleGitKeySampleGitKey0";
const otherKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISampleOtherKeySampleOtherKeySample0";
const credentials: Credentials = {
  githubApps: [
    { name: "client-org", server: "https://git.example.test" },
    { name: "elsewhere-org", server: "https://elsewhere.example.test" },
  ],
  sshKeys: [{ name: "sample-git-key", fingerprint: "SHA256:samplegit", publicKey: gitKey }],
  sshAgent: null,
};
const used: BackupRepository = {
  id: "example-backups",
  name: "Example backups",
  url: "git@git.example.test:example-org/backups.git",
  credential: null,
  workspaces: ["alpha"],
};
const idle: BackupRepository = {
  id: "client-backups",
  name: "Client backups",
  url: "https://git.example.test/client-org/backups.git",
  credential: { githubApp: "client-org" },
  workspaces: [],
};

interface Call {
  method: string;
  path: string;
  body: unknown;
}

function stubApi(repositories: BackupRepository[], offered: Credentials = credentials): Call[] {
  const calls: Call[] = [];
  let list = [...repositories];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = (init?.body ? JSON.parse(String(init.body)) : null) as {
        name: string;
        url: string;
        credential?: { sshKey: string } | { githubApp: string };
        check?: boolean;
      } | null;
      calls.push({ method, path, body });
      if (path === "/api/credentials") return Response.json(offered);
      if (path === "/api/backup-repositories" && method === "GET")
        return Response.json({ backupRepositories: list });
      if (path.endsWith("/credential") && method === "PUT" && body?.credential) {
        const id = path.split("/")[3];
        const chosen = body.credential;
        if ("sshKey" in chosen && chosen.sshKey === otherKey)
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
        const changed = list.find((r) => r.id === id)!;
        const next: BackupRepository = {
          ...changed,
          credential:
            "sshKey" in chosen
              ? { sshKey: chosen.sshKey, fingerprint: "SHA256:samplegit" }
              : { githubApp: chosen.githubApp },
        };
        if (!body.check) list = list.map((r) => (r.id === id ? next : r));
        return Response.json(next);
      }
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
        const made: BackupRepository = {
          id: "new-backups",
          name: body.name,
          url: body.url,
          credential:
            body.credential && "sshKey" in body.credential
              ? { sshKey: body.credential.sshKey, fingerprint: "SHA256:samplegit" }
              : null,
          workspaces: [],
        };
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
  // The agent's one key is the credential of an SSH URL, and its public
  // key is shown to give the Git host.
  expect(screen.getByRole("group", { name: "Public key" })).toHaveTextContent(gitKey);
  await userEvent.click(save);
  expect(await screen.findByRole("heading", { name: "New backups" })).toBeInTheDocument();
  const key = { sshKey: gitKey };
  expect(calls.filter((c) => c.method === "POST").map((c) => c.body)).toEqual([
    {
      name: "New backups",
      url: "git@git.example.test:o/readonly.git",
      credential: key,
      check: true,
    },
    { name: "New backups", url: "git@git.example.test:o/new.git", credential: key, check: true },
    { name: "New backups", url: "git@git.example.test:o/new.git", credential: key },
  ]);
  expect(await screen.findByText("SSH key sample-git-key · SHA256:samplegit")).toBeInTheDocument();
});

it("offers an https URL only the Apps that serve it, and none off their servers", async () => {
  const calls = stubApi([]);
  render(<BackupRepositoriesScreen />);
  await userEvent.click(
    (await screen.findAllByRole("button", { name: "Register repository" }))[0]!,
  );
  await userEvent.type(screen.getByRole("textbox", { name: "Display name" }), "Client");
  const url = screen.getByRole("textbox", { name: /Git URL/ });
  await userEvent.type(url, "https://git.example.test/client-org/backups.git");
  const select = await screen.findByRole("combobox", { name: "Credential" });
  expect(
    within(select)
      .getAllByRole("option")
      .map((o) => o.textContent),
  ).toEqual(["Choose a GitHub App…", "client-org"]);
  expect(select).toHaveValue("client-org");
  await userEvent.click(screen.getByRole("button", { name: "Validate" }));
  await userEvent.clear(url);
  await userEvent.type(url, "https://other.example.test/o/backups.git");
  expect(screen.queryByRole("combobox", { name: "Credential" })).not.toBeInTheDocument();
  expect(screen.getByText(/No configured GitHub App serves this URL/)).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Validate" }));
  expect(calls.filter((c) => c.method === "POST").map((c) => c.body)).toEqual([
    {
      name: "Client",
      url: "https://git.example.test/client-org/backups.git",
      credential: { githubApp: "client-org" },
      check: true,
    },
    { name: "Client", url: "https://other.example.test/o/backups.git", check: true },
  ]);
});

it("offers credentials for the URL forms the service accepts", async () => {
  stubApi([]);
  render(<BackupRepositoriesScreen />);
  await userEvent.click(
    (await screen.findAllByRole("button", { name: "Register repository" }))[0]!,
  );
  const url = screen.getByRole("textbox", { name: /Git URL/ });
  // An explicit default port names the App's server all the same.
  await userEvent.type(url, "https://git.example.test:443/client-org/backups.git");
  expect(await screen.findByRole("combobox", { name: "Credential" })).toHaveValue("client-org");
  // An scp-like address with no user name is SSH, as user git.
  await userEvent.clear(url);
  await userEvent.type(url, "git.example.test:example-org/backups.git");
  expect(screen.getByRole("group", { name: "Public key" })).toHaveTextContent(gitKey);
});

it("says why the agent offers no key", async () => {
  stubApi([], { githubApps: [], sshKeys: [], sshAgent: "the SSH agent could not be reached" });
  render(<BackupRepositoriesScreen />);
  await userEvent.click(
    (await screen.findAllByRole("button", { name: "Register repository" }))[0]!,
  );
  await userEvent.type(
    screen.getByRole("textbox", { name: /Git URL/ }),
    "git@git.example.test:o/r.git",
  );
  expect(await screen.findByText("the SSH agent could not be reached")).toBeInTheDocument();
});

it("changes a repository's credential only after a Validate with it passes", async () => {
  const offered = {
    ...credentials,
    sshKeys: [
      ...credentials.sshKeys,
      { name: "sample-other-key", fingerprint: "SHA256:sampleother", publicKey: otherKey },
    ],
  };
  const calls = stubApi([used], offered);
  render(<BackupRepositoriesScreen />);
  expect(
    await screen.findByText("Credential chosen by URL: every key the SSH agent offers"),
  ).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Change credential" }));
  const form = screen.getByRole("form", { name: "Change the credential of Example backups" });
  const select = within(form).getByRole("combobox", { name: "Credential" });
  const save = within(form).getByRole("button", { name: "Save" });
  expect(within(form).getByRole("button", { name: "Validate" })).toBeDisabled();
  await userEvent.selectOptions(select, otherKey);
  await userEvent.click(within(form).getByRole("button", { name: "Validate" }));
  expect(
    await within(form).findByText("write: Menagerie could not write to the repository"),
  ).toBeInTheDocument();
  expect(save).toBeDisabled();
  await userEvent.selectOptions(select, gitKey);
  await userEvent.click(within(form).getByRole("button", { name: "Validate" }));
  await waitFor(() => expect(save).toBeEnabled());
  await userEvent.click(save);
  expect(await screen.findByText("SSH key sample-git-key · SHA256:samplegit")).toBeInTheDocument();
  expect(calls.filter((c) => c.method === "PUT").map((c) => c.body)).toEqual([
    { credential: { sshKey: otherKey }, check: true },
    { credential: { sshKey: gitKey }, check: true },
    { credential: { sshKey: gitKey } },
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

// The templates screen registers a template from its address, shows a
// refusal by its check, upgrades a template to a newer version, edits it and
// removes one no workspace uses.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Template } from "../api/types.ts";
import { TemplatesScreen } from "./Templates.tsx";

const kit: Template = {
  id: "sample-kit",
  name: "Sample kit",
  address: "https://git.example.invalid/example-org/kit",
  currentVersion: "1.0.0",
  runnerImage: null,
  workspaces: ["alpha"],
};

interface Call {
  method: string;
  path: string;
  body: unknown;
}

/** Stubs the API with templates and answers, and records each call. */
function stubApi(answers: Record<string, () => Response>): Call[] {
  const calls: Call[] = [];
  let templates: Template[] = [kit];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push({ method, path, body: init?.body ? JSON.parse(String(init.body)) : null });
      const answer = answers[`${method} ${path}`];
      if (answer) {
        const response = answer();
        if (method === "POST" && path === "/api/templates" && response.status === 201) {
          templates = [...templates, (await response.clone().json()) as Template];
        }
        return response;
      }
      if (path === "/api/templates" && method === "GET") return Response.json({ templates });
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

describe("the templates screen", () => {
  it("registers a template from its address and lists it", async () => {
    const tools: Template = { ...kit, id: "tools", name: "Tools", workspaces: [] };
    const calls = stubApi({
      "POST /api/templates": () => Response.json(tools, { status: 201 }),
    });
    render(<TemplatesScreen />);
    expect(await screen.findByRole("heading", { name: "Sample kit" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Register template" }));
    await userEvent.type(screen.getByLabelText("Display name"), "Tools");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/tools");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("heading", { name: "Tools" })).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({
      name: "Tools",
      address: "gh:example-org/tools",
    });
    expect(screen.queryByLabelText("Address")).toBeNull();
  });

  it("shows a refusal by the check that failed", async () => {
    stubApi({
      "POST /api/templates": () =>
        Response.json(
          {
            error: {
              kind: "template-check-failed",
              check: "definition",
              message:
                "the template's files at version 1.0.0 have no .devcontainer/devcontainer.json",
            },
          },
          { status: 422 },
        ),
    });
    render(<TemplatesScreen />);
    await userEvent.click(await screen.findByRole("button", { name: "Register template" }));
    await userEvent.type(screen.getByLabelText("Display name"), "Bare");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/bare");
    await userEvent.type(screen.getByLabelText(/Runner image/), "alpine:3.22");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(
      await screen.findByText(/^definition: .*\.devcontainer\/devcontainer\.json/),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Address")).toHaveValue("gh:example-org/bare");
  });

  it("upgrades a template to a newer version and edits it", async () => {
    const calls = stubApi({
      "POST /api/templates/sample-kit/upgrades": () =>
        Response.json({ ...kit, currentVersion: "1.1.0" }),
      "PATCH /api/templates/sample-kit": () =>
        Response.json({ ...kit, currentVersion: "1.1.0", name: "Kit", runnerImage: "alpine:3.22" }),
    });
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    await userEvent.click(within(entry).getByRole("button", { name: "Upgrade template" }));
    await userEvent.type(within(entry).getByLabelText("Current version"), "1.1.0");
    await userEvent.click(within(entry).getByRole("button", { name: "Save" }));
    expect(await within(entry).findByText("1.1.0")).toBeInTheDocument();
    await userEvent.click(within(entry).getByRole("button", { name: "Edit" }));
    const name = within(entry).getByLabelText("Display name");
    await userEvent.clear(name);
    await userEvent.type(name, "Kit");
    await userEvent.type(within(entry).getByLabelText(/Runner image/), "alpine:3.22");
    await userEvent.click(within(entry).getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("heading", { name: "Kit" })).toBeInTheDocument();
    expect(calls.filter((c) => c.method !== "GET").map((c) => c.body)).toEqual([
      { version: "1.1.0" },
      { name: "Kit", runnerImage: "alpine:3.22" },
    ]);
  });

  it("names the workspaces a removal is refused for, and removes an unused one", async () => {
    let used = true;
    stubApi({
      "DELETE /api/templates/sample-kit": () =>
        used
          ? Response.json(
              {
                error: {
                  kind: "template-in-use",
                  message: "the template is used by workspace alpha; delete those workspaces first",
                },
              },
              { status: 409 },
            )
          : new Response(null, { status: 204 }),
    });
    render(<TemplatesScreen />);
    const remove = await screen.findByRole("button", { name: "Remove" });
    await userEvent.click(remove);
    expect(await screen.findByRole("alert", { name: "" })).toBeTruthy();
    expect(screen.getByText(/used by workspace alpha/)).toBeInTheDocument();
    used = false;
    await userEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Sample kit" })).toBeNull());
    expect(await screen.findByText("No registered templates.")).toBeInTheDocument();
  });
});

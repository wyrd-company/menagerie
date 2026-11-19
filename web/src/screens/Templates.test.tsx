// The templates screen registers a template from its address, validating
// it before it saves, shows a refusal by its check, shows each template's
// found versions and last check, checks one for new versions, upgrades a
// template to a newer found version, or its moved current version, the same
// way, edits it and removes one no workspace uses.
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Template } from "../api/types.ts";
import { TemplatesScreen } from "./Templates.tsx";

const found = (...versions: string[]) =>
  versions.map((version) => ({ version, tag: version, commit: `c${version}` }));

const kit: Template = {
  id: "sample-kit",
  name: "Sample kit",
  address: "https://git.example.invalid/example-org/kit",
  currentVersion: "1.0.0",
  runnerImage: null,
  workspaces: ["alpha"],
  newestVersion: "1.3.0",
  versions: found("1.0.0", "1.1.0", "1.2.0", "1.3.0"),
  currentMoved: false,
  lastCheck: { at: new Date(Date.now() - 12 * 60_000).toISOString(), failure: null },
};

interface Call {
  method: string;
  path: string;
  body: unknown;
}

type Body = { check?: boolean } | null;

/** Stubs the API with templates and answers, and records each call. */
function stubApi(answers: Record<string, (body: Body) => Response>): Call[] {
  const calls: Call[] = [];
  let templates: Template[] = [kit];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const body = (init?.body ? JSON.parse(String(init.body)) : null) as Body;
      calls.push({ method, path, body });
      const answer = answers[`${method} ${path}`];
      if (answer) {
        const response = answer(body);
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

/**
 * Stubs the API with the templates list, and holds each other call's reply
 * until the test releases it, the oldest first, recording each body.
 */
function holdReplies() {
  const releases: ((response: Response) => void)[] = [];
  const bodies: unknown[] = [];
  const release = (response: Response) => {
    const next = releases.shift();
    if (!next) throw new Error("no call waits for a reply");
    next(response);
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_path: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "GET") return Response.json({ templates: [kit] });
      bodies.push(init?.body ? JSON.parse(String(init.body)) : null);
      return new Promise<Response>((resolve) => releases.push(resolve));
    }),
  );
  return { release, bodies };
}

const refused = (check: string, message: string) =>
  Response.json({ error: { kind: "template-check-failed", check, message } }, { status: 422 });

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the templates screen", () => {
  it("validates a registration, then saves it and lists it", async () => {
    const tools: Template = { ...kit, id: "tools", name: "Tools", workspaces: [] };
    const calls = stubApi({
      "POST /api/templates": (body) =>
        body?.check ? Response.json(tools) : Response.json(tools, { status: 201 }),
    });
    render(<TemplatesScreen />);
    expect(await screen.findByRole("heading", { name: "Sample kit" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Register template" }));
    const save = screen.getByRole("button", { name: "Save" });
    expect(save).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Display name"), "Tools");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/tools");
    expect(save).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByText("Checks passed · 1.0.0")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("heading", { name: "Tools" })).toBeInTheDocument();
    expect(calls.filter((c) => c.method === "POST").map((c) => c.body)).toEqual([
      { name: "Tools", address: "gh:example-org/tools", check: true },
      { name: "Tools", address: "gh:example-org/tools" },
    ]);
    expect(screen.queryByLabelText("Address")).toBeNull();
  });

  it("disables Save again on any edit after a passing Validate", async () => {
    stubApi({ "POST /api/templates": () => Response.json(kit) });
    render(<TemplatesScreen />);
    await userEvent.click(await screen.findByRole("button", { name: "Register template" }));
    await userEvent.type(screen.getByLabelText("Display name"), "Kit");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/kit");
    for (const field of ["Display name", "Address", /Runner image/]) {
      await userEvent.click(screen.getByRole("button", { name: "Validate" }));
      expect(await screen.findByText("Checks passed · 1.0.0")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
      await userEvent.type(screen.getByLabelText(field), "x");
      expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
      expect(screen.queryByText("Checks passed · 1.0.0")).toBeNull();
    }
  });

  it("shows a refusal by the check that failed, marking the address only for its check", async () => {
    let check = "definition";
    stubApi({
      "POST /api/templates": () =>
        check === "definition"
          ? refused(
              "definition",
              "the template's files at version 1.0.0 have no .devcontainer/devcontainer.json",
            )
          : refused("address", "the address is refused: the address holds a password"),
    });
    render(<TemplatesScreen />);
    await userEvent.click(await screen.findByRole("button", { name: "Register template" }));
    await userEvent.type(screen.getByLabelText("Display name"), "Bare");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/bare");
    await userEvent.type(screen.getByLabelText(/Runner image/), "alpine:3.22");
    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(
      await screen.findByText(/^definition: .*\.devcontainer\/devcontainer\.json/),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Address")).toHaveValue("gh:example-org/bare");
    expect(screen.getByLabelText("Address")).not.toHaveAttribute("aria-invalid");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    check = "address";
    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    expect(await screen.findByText(/^address: /)).toBeInTheDocument();
    expect(screen.getByLabelText("Address")).toHaveAttribute("aria-invalid", "true");
    // An edit clears the refusal.
    await userEvent.type(screen.getByLabelText("Address"), "x");
    expect(screen.queryByText(/^address: /)).toBeNull();
  });

  it("shows a Save's own refusal: it checks again, and Save waits for another Validate", async () => {
    stubApi({
      "POST /api/templates": (body) =>
        body?.check
          ? Response.json(kit)
          : refused(
              "fetch",
              "the template could not be fetched: the Git host has no such repository",
            ),
    });
    render(<TemplatesScreen />);
    await userEvent.click(await screen.findByRole("button", { name: "Register template" }));
    await userEvent.type(screen.getByLabelText("Display name"), "Kit");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/kit");
    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    await userEvent.click(await screen.findByRole("button", { name: "Save" }));
    expect(await screen.findByText(/^fetch: /)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("enables Save only for the values a Validate passed, even when an edit overtakes it", async () => {
    const { release, bodies } = holdReplies();
    const tools = { ...kit, id: "tools", name: "Tools" };
    render(<TemplatesScreen />);
    await userEvent.click(await screen.findByRole("button", { name: "Register template" }));
    await userEvent.type(screen.getByLabelText("Display name"), "Tools");
    await userEvent.type(screen.getByLabelText("Address"), "gh:example-org/tools");
    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    // While the check of gh:example-org/tools runs, the address changes.
    await userEvent.type(screen.getByLabelText("Address"), "-other");
    release(Response.json(tools));
    expect(await screen.findByRole("button", { name: "Validate" })).toBeEnabled();
    expect(screen.queryByText("Checks passed · 1.0.0")).toBeNull();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    // A Validate of the values the form holds counts.
    await userEvent.click(screen.getByRole("button", { name: "Validate" }));
    release(Response.json(tools));
    expect(await screen.findByText("Checks passed · 1.0.0")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
    expect(bodies).toEqual([
      { name: "Tools", address: "gh:example-org/tools", check: true },
      { name: "Tools", address: "gh:example-org/tools-other", check: true },
    ]);
  });

  it("counts an upgrade's Validate reply, a pass or a refusal, only for the version it sent", async () => {
    const { release, bodies } = holdReplies();
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    await userEvent.click(within(entry).getByRole("button", { name: "Upgrade template" }));
    const version = within(entry).getByLabelText("Current version");
    const validate = async (answer: Response) => {
      await userEvent.click(within(entry).getByRole("button", { name: "Validate" }));
      return () => release(answer);
    };
    await userEvent.selectOptions(version, "1.1.0");
    // A pass for 1.1.0 arrives once the version reads 1.2.0.
    let reply = await validate(Response.json({ ...kit, currentVersion: "1.1.0" }));
    await userEvent.selectOptions(version, "1.2.0");
    reply();
    expect(await within(entry).findByRole("button", { name: "Validate" })).toBeEnabled();
    expect(within(entry).queryByText(/^Checks passed/)).toBeNull();
    expect(within(entry).getByRole("button", { name: "Save" })).toBeDisabled();
    // A refusal of 1.2.0 arrives once the version reads 1.3.0.
    reply = await validate(refused("version", "1.2.0 is not a version of the repository"));
    await userEvent.selectOptions(version, "1.3.0");
    reply();
    expect(await within(entry).findByRole("button", { name: "Validate" })).toBeEnabled();
    expect(within(entry).queryByText(/^version: /)).toBeNull();
    // A pass of 1.3.0, the version the form holds, counts.
    reply = await validate(Response.json({ ...kit, currentVersion: "1.3.0" }));
    reply();
    expect(await within(entry).findByText("Checks passed · 1.3.0")).toBeInTheDocument();
    expect(within(entry).getByRole("button", { name: "Save" })).toBeEnabled();
    expect(bodies).toEqual([
      { version: "1.1.0", check: true },
      { version: "1.2.0", check: true },
      { version: "1.3.0", check: true },
    ]);
  });

  it("validates a template's upgrade, then saves it, and edits it", async () => {
    const calls = stubApi({
      "POST /api/templates/sample-kit/upgrades": () =>
        Response.json({ ...kit, currentVersion: "1.1.0" }),
      "PATCH /api/templates/sample-kit": () =>
        Response.json({ ...kit, currentVersion: "1.1.0", name: "Kit", runnerImage: "alpine:3.22" }),
    });
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    await userEvent.click(within(entry).getByRole("button", { name: "Upgrade template" }));
    expect(within(entry).getByRole("button", { name: "Save" })).toBeDisabled();
    await userEvent.selectOptions(within(entry).getByLabelText("Current version"), "1.1.0");
    await userEvent.click(within(entry).getByRole("button", { name: "Validate" }));
    expect(await within(entry).findByText("Checks passed · 1.1.0")).toBeInTheDocument();
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
      { version: "1.1.0", check: true },
      { version: "1.1.0" },
      { name: "Kit", runnerImage: "alpine:3.22" },
    ]);
  });

  it("shows the found versions and the last check, and offers only newer found versions", async () => {
    stubApi({});
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    expect(within(entry).getByText("Checked 12 minutes ago")).toBeInTheDocument();
    expect(within(entry).getByText("Current").querySelector("strong")).toHaveTextContent("1.0.0");
    expect(within(entry).getByText("Newest").querySelector("strong")).toHaveTextContent("1.3.0");
    expect(within(entry).getByText("New version")).toBeInTheDocument();
    expect(within(entry).queryByRole("button", { name: "Update version" })).toBeNull();
    await userEvent.click(within(entry).getByRole("button", { name: "Upgrade template" }));
    const select = within(entry).getByLabelText<HTMLSelectElement>("Current version");
    expect([...select.options].map((o) => o.value)).toEqual(["1.3.0", "1.2.0", "1.1.0"]);
    expect(select.value).toBe("1.3.0");
  });

  it("offers no template upgrade while no newer version is found", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        Response.json({
          templates: [
            { ...kit, newestVersion: "1.0.0", versions: found("0.9.0", "1.0.0") },
            {
              ...kit,
              id: "never",
              name: "Never checked",
              newestVersion: null,
              versions: [],
              lastCheck: null,
            },
          ],
        }),
      ),
    );
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    expect(within(entry).queryByRole("button", { name: "Upgrade template" })).toBeNull();
    expect(within(entry).queryByRole("button", { name: "Update version" })).toBeNull();
    expect(within(entry).queryByText("New version")).toBeNull();
    const never = screen.getByRole("heading", { name: "Never checked" }).closest("li")!;
    expect(within(never).getByText("Not checked yet")).toBeInTheDocument();
    expect(within(never).getByText("Newest").querySelector("strong")).toHaveTextContent(
      "none found",
    );
  });

  it("checks a template for new versions on request, showing the check while it runs", async () => {
    const { release } = holdReplies();
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    const check = within(entry).getByRole("button", { name: "Check versions" });
    await userEvent.click(check);
    expect(within(entry).getByText("Checking versions…")).toBeInTheDocument();
    expect(check).toBeDisabled();
    release(
      Response.json({
        ...kit,
        newestVersion: "1.4.0",
        versions: found("1.0.0", "1.4.0"),
        lastCheck: { at: new Date().toISOString(), failure: null },
      }),
    );
    expect(await within(entry).findByText("Checked just now")).toBeInTheDocument();
    expect(within(entry).getByText("Newest").querySelector("strong")).toHaveTextContent("1.4.0");
    expect(check).toBeEnabled();
  });

  it("shows a failed check, keeping the versions found before", async () => {
    const { release } = holdReplies();
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    await userEvent.click(within(entry).getByRole("button", { name: "Check versions" }));
    release(
      Response.json({
        ...kit,
        lastCheck: { at: new Date().toISOString(), failure: "template-service-unavailable" },
      }),
    );
    const line = await within(entry).findByText(
      "Check failed just now: template service is unavailable. Previously found versions are kept.",
    );
    expect(line).toHaveClass("form-error");
    expect(within(entry).getByText("Newest").querySelector("strong")).toHaveTextContent("1.3.0");
  });

  it("takes a moved current version again through Update version", async () => {
    const moved = { ...kit, currentVersion: "1.3.0", currentMoved: true };
    const calls: Call[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (path: string, init?: RequestInit) => {
        const method = init?.method ?? "GET";
        const body = (init?.body ? JSON.parse(String(init.body)) : null) as Body;
        calls.push({ method, path, body });
        if (method === "GET") return Response.json({ templates: [moved] });
        // A check alone answers the template at the version, a save stores it.
        return Response.json({ ...moved, currentMoved: false });
      }),
    );
    render(<TemplatesScreen />);
    const entry = (await screen.findByRole("heading", { name: "Sample kit" })).closest("li")!;
    expect(
      within(entry).getByText(
        "Current tag moved. Validate and save this version again before using it.",
      ),
    ).toBeInTheDocument();
    expect(within(entry).getByText("Tag moved")).toBeInTheDocument();
    expect(within(entry).queryByRole("button", { name: "Upgrade template" })).toBeNull();
    await userEvent.click(within(entry).getByRole("button", { name: "Update version" }));
    const select = within(entry).getByLabelText<HTMLSelectElement>("Current version");
    expect([...select.options].map((o) => o.value)).toEqual(["1.3.0"]);
    await userEvent.click(within(entry).getByRole("button", { name: "Validate" }));
    expect(await within(entry).findByText("Checks passed · 1.3.0")).toBeInTheDocument();
    await userEvent.click(within(entry).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(within(entry).queryByText(/^Current tag moved/)).toBeNull());
    expect(within(entry).queryByRole("button", { name: "Update version" })).toBeNull();
    expect(calls.filter((c) => c.method === "POST").map((c) => c.body)).toEqual([
      { version: "1.3.0", check: true },
      { version: "1.3.0" },
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

// Question ids and workspace names come from outside the browser. Names that
// are also Object.prototype members must behave like any other name.
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { BatchDocument, Submission } from "../api/types.ts";
import { WorkspaceList } from "../screens/WorkspaceList.tsx";
import { StatusBadge } from "../ui/StatusBadge.tsx";
import { JobConsole } from "../jobs/JobConsole.tsx";
import { initialJobView } from "../jobs/jobView.ts";
import { buildSubmission, initialForm, serializeSubmission } from "./answers.ts";
import { readField } from "./fields.ts";
import { InterviewForm } from "./InterviewForm.tsx";

const names = ["constructor", "__proto__", "toString", "hasOwnProperty"];

const batch = (id: string, errors?: Record<string, string[]>): BatchDocument => {
  const doc = JSON.parse(
    `{"protocol":1,"status":"questions","context":{"target":"/srv/workspaces/sample","template":"sample","commit":null},"schema":{"type":"object","properties":{${JSON.stringify(id)}:{"type":"string","x-toha-type":"text","title":"Sample"}},"required":[${JSON.stringify(id)}]},"messages":[]}`,
  ) as BatchDocument;
  if (errors) doc.errors = errors;
  return doc;
};

describe.each(names)("a question named %s", (id) => {
  it("renders, and is answered under its own name", async () => {
    const onSubmit = vi.fn<(s: Submission) => void>();
    render(
      <InterviewForm
        document={batch(id)}
        ordinal={1}
        submitting={false}
        submitError={null}
        onSubmit={onSubmit}
      />,
    );
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/^Sample/), "sample");
    await user.click(screen.getByRole("button", { name: "Send answers" }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
    const submission = onSubmit.mock.calls[0]![0];
    expect(serializeSubmission(submission)).toBe(
      `{"template":"sample","answers":{${JSON.stringify(id)}:"sample"}}`,
    );
  });

  it("renders when the batch carries errors for other questions", () => {
    render(
      <InterviewForm
        document={batch(id, { other: ["is not a question in this template"] })}
        ordinal={1}
        submitting={false}
        submitError={null}
        onSubmit={() => {}}
      />,
    );
    expect(screen.getByLabelText(/^Sample/)).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "other: is not a question in this template",
    );
  });

  it("builds a submission with the answer as an own member", () => {
    const fields = [readField(id, { type: "string", "x-toha-type": "text" }, true)];
    const form = initialForm(fields);
    const changed = {
      ...form,
      [id]: { value: { kind: "text" as const, text: "sample" }, dirty: true },
    };
    const submission = buildSubmission("sample", fields, changed);
    expect(Object.hasOwn(submission.answers, id)).toBe(true);
    expect(serializeSubmission(submission)).toContain(`${JSON.stringify(id)}:"sample"`);
  });
});

describe.each(names)("a workspace or state named %s", (name) => {
  it("keeps the actions of an idle workspace", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (path: string) =>
        Response.json(
          path === "/api/jobs"
            ? { jobs: [] }
            : { workspaces: [{ name, state: "stopped", containerId: null, activeJob: null }] },
        ),
      ),
    );
    render(<WorkspaceList jobId={null} />);
    expect(await screen.findByRole("button", { name: "Start" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Rebuild" })).toBeEnabled();
    expect(screen.queryByRole("alert")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("shows an unknown state or operation verbatim", () => {
    render(<StatusBadge state={name} />);
    expect(screen.getByText(name)).toBeInTheDocument();
    const job = {
      jobId: "j",
      workspace: "sample",
      operation: name,
      state: "running",
      phase: "",
      createdAt: "",
      finishedAt: null,
      lastEventId: 0,
      error: null,
    };
    render(<JobConsole view={{ ...initialJobView, job }} onReconnect={() => {}} />);
    expect(
      screen.getByRole("heading", { name: new RegExp(name.replace(/_/g, "_")) }),
    ).toBeInTheDocument();
  });
});

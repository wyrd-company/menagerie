import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { BatchDocument, Submission } from "../api/types.ts";
import rejected from "../../test/fixtures/toha/documents/create-batch-1-rejected.json";
import batch1 from "../../test/fixtures/toha/documents/create-batch-1.json";
import batch2 from "../../test/fixtures/toha/documents/create-batch-2.json";
import { InterviewForm } from "./InterviewForm.tsx";

function setup(document: unknown) {
  const onSubmit = vi.fn<(s: Submission) => void>();
  const props = { ordinal: 1, submitting: false, submitError: null, onSubmit };
  const view = render(<InterviewForm document={document as BatchDocument} {...props} />);
  const rerender = (doc: unknown) =>
    view.rerender(<InterviewForm document={doc as BatchDocument} {...props} />);
  return { onSubmit, rerender, user: userEvent.setup() };
}

describe("InterviewForm", () => {
  it("renders every question of the batch with an accessible control", () => {
    setup(batch1);
    expect(screen.getByLabelText(/^Title/)).toHaveAttribute("type", "text");
    expect(screen.getByLabelText(/^Notes/).tagName).toBe("TEXTAREA");
    expect(screen.getByLabelText(/^Remote repository/)).toHaveAttribute("type", "url");
    expect(screen.getByLabelText(/^Settings document/).tagName).toBe("TEXTAREA");
    expect(
      within(screen.getByRole("group", { name: /Enable the sample service/ })).getAllByRole(
        "radio",
      ),
    ).toHaveLength(2);
    expect(
      within(screen.getByRole("group", { name: /Verbose logging/ })).getByRole("radio", {
        name: "No answer",
      }),
    ).toBeInTheDocument();
    expect(
      within(screen.getByRole("group", { name: /^Size/ })).getByRole("radio", { name: "medium" }),
    ).toBeChecked();
    expect(
      within(screen.getByRole("group", { name: /^Palette/ })).getByRole("radio", {
        name: "No answer",
      }),
    ).toBeInTheDocument();
    expect(
      within(screen.getByRole("group", { name: /^Features/ })).getByRole("checkbox", {
        name: "alpha",
      }),
    ).toBeChecked();
    expect(screen.getByRole("button", { name: /Add label/ })).toBeEnabled();
  });

  it("blocks a submission the browser can already see is incomplete", async () => {
    const { onSubmit, user } = setup(batch1);
    await user.click(screen.getByRole("button", { name: "Send answers" }));
    expect(onSubmit).not.toHaveBeenCalled();
    const summary = screen.getByRole("alert");
    expect(summary).toHaveTextContent("Title: is required");
    expect(summary).toHaveFocus();
    expect(screen.getByLabelText(/^Title/)).toHaveAttribute("aria-invalid", "true");
  });

  it("submits typed answers with the keyboard, leaving untouched questions out", async () => {
    const { onSubmit, user } = setup(batch1);
    await user.type(screen.getByLabelText(/^Title/), "Sample project");
    await user.click(screen.getByRole("radio", { name: "large" }));
    await user.click(screen.getByRole("button", { name: /Add label/ }));
    await user.keyboard("one{Enter}two");
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(onSubmit).toHaveBeenCalledWith({
      template: batch1.context.template,
      answers: { title: "Sample project", size: "large", labels: ["one", "two"] },
    });
  });

  it("keeps the person's answers when the same batch comes back rejected, and shows Toha's errors", async () => {
    const { rerender, user } = setup(batch1);
    await user.type(screen.getByLabelText(/^Title/), "Sample project");
    await user.click(screen.getByRole("button", { name: /Add label/ }));
    await user.keyboard("Bad Label");
    rerender(rejected);
    expect(screen.getByLabelText(/^Title/)).toHaveValue("Sample project");
    expect(screen.getByRole("textbox", { name: "Label 1" })).toHaveValue("Bad Label");
    const summary = screen.getByRole("alert");
    expect(summary).toHaveFocus();
    expect(summary).toHaveTextContent("Label: each item must match ^[a-z0-9-]+$");
    expect(summary).toHaveTextContent(
      'Settings document: schema https://schemas.example.test/sample-settings/1: "name" is a required property',
    );
    expect(summary).toHaveTextContent("2 answers need attention.");
    expect(screen.getByRole("textbox", { name: "Label 1" }).closest("fieldset")).toHaveClass(
      "is-invalid",
    );
    await user.clear(screen.getByRole("textbox", { name: "Label 1" }));
    await user.keyboard("good-label");
    expect(screen.getByRole("alert")).not.toHaveTextContent("Label:");
    expect(screen.getByRole("alert")).toHaveTextContent("One answer needs attention.");
  });

  it("starts the next batch from its own defaults and shows its messages", () => {
    const { rerender } = setup(batch1);
    rerender(batch2);
    expect(screen.getByLabelText(/^Folder name/)).toHaveValue("sample-project");
    expect(screen.getByRole("list", { name: "Messages from the template" })).toHaveTextContent(
      "Sample kit for Sample project.",
    );
  });

  it("shows errors for ids outside the batch and Toha warnings", () => {
    setup({
      ...batch2,
      messages: ['warning: answer for "palette" was not used: the question was skipped'],
      errors: { stray: ["is not a question in this template"] },
    });
    expect(screen.getByRole("alert")).toHaveTextContent(
      "stray: is not a question in this template",
    );
    expect(screen.getByText(/answer for "palette" was not used/).closest("li")).toHaveClass(
      "warning",
    );
  });
});

describe("InterviewForm while submitting", () => {
  it("sends nothing on Ctrl+Enter", async () => {
    const onSubmit = vi.fn<(s: Submission) => void>();
    render(
      <InterviewForm
        document={batch2 as unknown as BatchDocument}
        ordinal={2}
        submitting
        submitError={null}
        onSubmit={onSubmit}
      />,
    );
    const user = userEvent.setup();
    await user.click(screen.getByLabelText(/^Folder name/));
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(onSubmit).not.toHaveBeenCalled();
  });
});

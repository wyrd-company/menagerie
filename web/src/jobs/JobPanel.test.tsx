import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { parseInterviewReply } from "../api/client.ts";
import { initialJobView, type JobView } from "./jobView.ts";

const view: { current: JobView } = { current: initialJobView };
vi.mock("./useJob.ts", () => ({
  useJob: () => ({ view: view.current, applyReply: () => {}, reconnect: () => {} }),
}));
const { JobPanel } = await import("./JobPanel.tsx");

// A document question whose default the producer wrote with 17 significant digits.
const replyText = `{"session":{"n":1},"state":"questions","document":{"protocol":1,"status":"questions","context":{"target":"/srv/workspaces/sample","template":"sample","commit":null},"schema":{"type":"object","properties":{"title":{"type":"string","x-toha-type":"text","title":"Title","default":"sample"},"doc":{"type":"object","x-toha-type":"document","title":"Doc","default":{"count":9007199254740993}}},"required":["title"]},"messages":[]},"warnings":[]}`;

afterEach(() => vi.unstubAllGlobals());

describe("JobPanel", () => {
  it("shows a document default with the producer's own digits", () => {
    view.current = {
      ...initialJobView,
      state: "waiting-for-answers",
      interview: parseInterviewReply(replyText),
      batch: 1,
    };
    render(<JobPanel jobId="job-1" />);
    expect(screen.getByLabelText(/^Doc/)).toHaveValue('{"count":9007199254740993}');
  });

  it("sends one answers request when submissions arrive in the same frame", () => {
    view.current = {
      ...initialJobView,
      state: "waiting-for-answers",
      interview: parseInterviewReply(replyText),
      batch: 1,
    };
    const fetch = vi.fn(() => new Promise<Response>(() => {}));
    vi.stubGlobal("fetch", fetch);
    render(<JobPanel jobId="job-1" />);
    const form = screen.getByRole("button", { name: "Send answers" }).closest("form")!;
    // Both keydowns are dispatched before React re-renders with `submitting`.
    fireEvent.keyDown(form, { key: "Enter", ctrlKey: true });
    fireEvent.keyDown(form, { key: "Enter", ctrlKey: true });
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});

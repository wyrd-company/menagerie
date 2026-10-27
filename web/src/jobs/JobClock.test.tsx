import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Job } from "../api/types.ts";
import { formatElapsed, formatLimit, JobClock } from "./JobClock.tsx";
import { initialJobView, type JobView } from "./jobView.ts";

const job: Job = {
  jobId: "job-1",
  workspace: "sample-space",
  operation: "rebuild",
  state: "running",
  phase: "build",
  createdAt: "2030-01-01T00:00:00Z",
  finishedAt: null,
  lastEventId: 2,
  error: null,
  deadline: null,
  limit: null,
};

const clock = () =>
  screen
    .getAllByRole("listitem")
    .map((item) => item.textContent)
    .join(" | ");

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2030-01-01T00:03:07Z"));
});
afterEach(() => vi.useRealTimers());

describe("JobClock", () => {
  it("shows the elapsed time alone without a limit, and keeps counting", () => {
    render(<JobClock view={{ ...initialJobView, job, state: "running", phase: "build" }} />);
    expect(clock()).toBe("Elapsed 3m 07s");
    act(() => vi.advanceTimersByTime(60_000));
    expect(clock()).toBe("Elapsed 4m 07s");
  });

  it("shows the limit that ends first and the time left", () => {
    const view: JobView = {
      ...initialJobView,
      job,
      state: "running",
      phase: "build",
      deadline: "2030-01-01T00:45:00Z",
      limit: { seconds: 2700, scope: "phase" },
    };
    render(<JobClock view={view} />);
    expect(clock()).toBe("Elapsed 3m 07s | Limit 45m (build phase) | Left 41m 53s");
  });

  it("says the limit is paused while the job waits for answers", () => {
    const view: JobView = {
      ...initialJobView,
      job,
      state: "waiting-for-answers",
      phase: "interview",
      limit: { seconds: 7200, scope: "job" },
    };
    render(<JobClock view={view} />);
    expect(clock()).toBe("Elapsed 3m 07s | Limit 2h (job) | Paused while waiting for answers");
  });

  it("stops at the finish time once the job has ended", () => {
    const ended = { ...job, state: "failed", finishedAt: "2030-01-01T01:00:00Z" };
    render(
      <JobClock
        view={{
          ...initialJobView,
          job: ended,
          state: "failed",
          result: { state: "failed", error: null },
        }}
      />,
    );
    expect(clock()).toBe("Elapsed 1h 00m");
  });
});

describe("formatting", () => {
  it("writes elapsed time and limits in whole units", () => {
    expect(formatElapsed(-5)).toBe("0s");
    expect(formatElapsed(45_900)).toBe("45s");
    expect(formatElapsed(3_723_000)).toBe("1h 02m");
    expect(formatLimit(5400)).toBe("1h 30m");
    expect(formatLimit(3630)).toBe("1h 30s");
    expect(formatLimit(0.1)).toBe("100ms");
  });
});

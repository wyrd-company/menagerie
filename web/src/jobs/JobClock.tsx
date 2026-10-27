import { useEffect, useState } from "react";
import { isTerminal, type JobView } from "./jobView.ts";

/** How long a job has run and, under a time limit, the limit and the time left. */
export function JobClock({ view }: { view: JobView }) {
  const terminal = isTerminal(view);
  const now = useNow(!terminal);
  const { job, limit, deadline } = view;
  if (!job) return null;
  const end = job.finishedAt ? Date.parse(job.finishedAt) : now;
  const scope = limit?.scope === "phase" ? `${view.phase} phase` : "job";
  return (
    <ul className="job-clock" aria-label="Job time">
      <li>
        Elapsed <span className="mono">{formatElapsed(end - Date.parse(job.createdAt))}</span>
      </li>
      {limit && !terminal ? (
        <li>
          Limit <span className="mono">{formatLimit(limit.seconds)}</span> ({scope})
        </li>
      ) : null}
      {limit && deadline && !terminal ? (
        <li>
          Left <span className="mono">{formatElapsed(Date.parse(deadline) - now)}</span>
        </li>
      ) : null}
      {limit && !deadline && !terminal ? <li>Paused while waiting for answers</li> : null}
    </ul>
  );
}

/** The current time, refreshed every second while active. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/** Whole seconds as "1h 02m", "3m 07s" or "45s"; never negative. */
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const two = (n: number) => String(n).padStart(2, "0");
  if (h > 0) return `${h}h ${two(m)}m`;
  if (m > 0) return `${m}m ${two(s)}s`;
  return `${s}s`;
}

/** A limit as its configured units, such as "2h", "1h 30m" or "45m"; below a second in ms. */
export function formatLimit(seconds: number): string {
  if (seconds < 1) return `${Math.round(seconds * 1000)}ms`;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.round(seconds % 60);
  return [h ? `${h}h` : "", m ? `${m}m` : "", s ? `${s}s` : ""].filter(Boolean).join(" ");
}

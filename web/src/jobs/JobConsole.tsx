import { useEffect, useRef, useState } from "react";
import type { ApplyReply } from "../api/types.ts";
import { own } from "../own.ts";
import { StatusBadge } from "../ui/StatusBadge.tsx";
import { JobClock } from "./JobClock.tsx";
import { isTerminal, type JobView } from "./jobView.ts";

interface Props {
  view: JobView;
  onReconnect: () => void;
  heading?: string;
}

const operationVerb: Record<string, string> = {
  create: "Create",
  upgrade: "Upgrade",
  start: "Start",
  stop: "Stop",
  rebuild: "Rebuild",
};

/** Live phases, log lines and the outcome of one job. */
export function JobConsole({ view, onReconnect, heading }: Props) {
  const { job } = view;
  const logRef = useRef<HTMLOListElement>(null);
  const [follow, setFollow] = useState(true);

  useEffect(() => {
    const el = logRef.current;
    if (el && follow) el.scrollTop = el.scrollHeight;
  }, [view.logs.length, follow]);

  const operation = job ? (own(operationVerb, job.operation) ?? job.operation) : "Job";
  const terminal = isTerminal(view);
  const error = view.result?.error ?? job?.error ?? null;
  const phases = view.phases.length > 0 || view.phase === "" ? view.phases : [view.phase];

  return (
    <section className="console" aria-label={heading ?? "Job console"}>
      <header className="console-head">
        <div>
          <p className="eyebrow">{heading ?? "Job"}</p>
          <h2 className="console-title">
            {operation} <span className="mono">{job?.workspace ?? "…"}</span>
          </h2>
        </div>
        <StatusBadge state={view.state || "running"} />
      </header>

      <JobClock view={view} />

      {phases.length > 0 ? (
        <ol className="phase-rail" aria-label="Phases">
          {phases.map((phase) => {
            const current = phase === view.phase && !terminal;
            return (
              <li
                key={phase}
                className={current ? "is-current" : "is-done"}
                aria-current={current ? "step" : undefined}
              >
                {phase}
              </li>
            );
          })}
        </ol>
      ) : null}

      {view.connection === "lost" && !terminal ? (
        <div className="notice notice-warn" role="status">
          <p>The live stream stopped. The job keeps running in Menagerie.</p>
          <button type="button" onClick={onReconnect}>
            Reconnect
          </button>
        </div>
      ) : null}
      {view.connection === "gone" ? (
        <div className="notice notice-warn" role="status">
          <p>
            Menagerie no longer knows this job. A restart drops job history; the workspace list
            shows the current state.
          </p>
        </div>
      ) : null}

      <div className="tape">
        <div className="tape-head">
          <span>Log</span>
          <label className="follow">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            Follow
          </label>
        </div>
        <ol className="tape-lines" ref={logRef} role="log" aria-label="Job log" tabIndex={0}>
          {view.logs.length === 0 ? <li className="tape-empty">No output yet.</li> : null}
          {view.logs.map((line, index) => (
            <li key={line.id}>
              <span className="tape-n" aria-hidden="true">
                {index + 1}
              </span>
              <span className="tape-text">{line.text}</span>
            </li>
          ))}
        </ol>
      </div>

      {terminal ? (
        <div className={`outcome outcome-${view.state}`} role="status">
          {view.state === "succeeded" ? <p className="outcome-title">Completed.</p> : null}
          {view.state === "ended" ? (
            <p className="outcome-title">The interview ended. Nothing was written.</p>
          ) : null}
          {view.state === "failed" ? <p className="outcome-title">Failed.</p> : null}
          {error ? (
            <p className="outcome-error">
              <span className="mono">{error.kind}</span> {error.message}
            </p>
          ) : null}
          {view.result?.apply ? <ApplySummary apply={view.result.apply} /> : null}
        </div>
      ) : null}
    </section>
  );
}

function ApplySummary({ apply }: { apply: ApplyReply }) {
  return (
    <div className="apply">
      <p>
        Template outcome: <strong className="mono">{apply.outcome}</strong>
        {apply.recordPath ? (
          <>
            {" "}
            · record <span className="mono">{apply.recordPath}</span>
          </>
        ) : null}
      </p>
      {apply.conflicts && apply.conflicts.length > 0 ? (
        <div className="notice notice-warn">
          <p>These paths were written with merge conflicts and need a manual fix:</p>
          <ul className="mono">
            {apply.conflicts.map((c) => (
              <li key={c}>{c}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {apply.messages && apply.messages.length > 0 ? (
        <ul className="messages">
          {apply.messages.map((m, i) => (
            <li key={i} className="message">
              {m}
            </li>
          ))}
        </ul>
      ) : null}
      {apply.files && apply.files.length > 0 ? (
        <table className="files">
          <caption>Files</caption>
          <thead>
            <tr>
              <th scope="col">Path</th>
              <th scope="col">Action</th>
            </tr>
          </thead>
          <tbody>
            {apply.files.map((f) => (
              <tr key={f.path}>
                <td className="mono">{f.path}</td>
                <td>{f.action}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      {apply.hooks && apply.hooks.length > 0 ? (
        <p className="hooks">
          Hooks:{" "}
          {apply.hooks.map((h, i) => (
            <span key={i} className={h.success ? "hook ok" : "hook bad"}>
              {h.id ?? `#${i + 1}`} exit {h.exitCode ?? "–"}
            </span>
          ))}
        </p>
      ) : null}
    </div>
  );
}

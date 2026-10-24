import { useCallback, useEffect, useState, type CSSProperties } from "react";
import { ApiError, listJobs, listWorkspaces, startJob } from "../api/client.ts";
import type { Job, Operation, Workspace } from "../api/types.ts";
import { JobPanel } from "../jobs/JobPanel.tsx";
import { navigate, paths } from "../router.ts";
import { Link } from "../ui/Link.tsx";
import { StateStamp } from "../ui/StateStamp.tsx";

interface RowError {
  message: string;
  activeJobId?: string;
}

const operationLabel: Record<string, string> = {
  create: "creating",
  upgrade: "upgrading",
  start: "starting",
  stop: "stopping",
  rebuild: "rebuilding",
};

function useFleet() {
  const [workspaces, setWorkspaces] = useState<Workspace[] | null>(null);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(false);

  // Reads the folders, containers and jobs again. State changes only after the reads.
  const refresh = useCallback(
    () =>
      Promise.all([listWorkspaces(), listJobs().catch(() => null)])
        .then(([ws, js]) => {
          setWorkspaces(ws);
          if (js) setJobs(js);
          setError(null);
        })
        .catch((e: unknown) =>
          setError(e instanceof ApiError ? e : new ApiError(0, null, String(e))),
        )
        .finally(() => setLoading(false)),
    [],
  );

  useEffect(() => {
    void refresh();
    // The list is refreshed when the person returns to the tab, not on a timer.
    const onVisible = () => {
      if (document.visibilityState === "visible") void refresh();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, [refresh]);

  const reload = useCallback(() => {
    setLoading(true);
    void refresh();
  }, [refresh]);

  return { workspaces, jobs, error, loading, refresh, reload };
}

export function WorkspaceList({ jobId }: { jobId: string | null }) {
  const { workspaces, jobs, error, loading, refresh, reload } = useFleet();
  const [pending, setPending] = useState<Record<string, Operation>>({});
  const [rowErrors, setRowErrors] = useState<Record<string, RowError>>({});
  const [confirming, setConfirming] = useState<string | null>(null);

  const run = async (workspace: string, operation: Operation) => {
    setConfirming(null);
    setPending((p) => ({ ...p, [workspace]: operation }));
    setRowErrors(({ [workspace]: _, ...rest }) => rest);
    try {
      const job = await startJob({ operation, workspace });
      navigate(paths.list(job.jobId));
    } catch (e) {
      const err = e instanceof ApiError ? e : new ApiError(0, null, String(e));
      const rowError: RowError = { message: err.message };
      if (err.body?.activeJobId) rowError.activeJobId = err.body.activeJobId;
      setRowErrors((r) => ({ ...r, [workspace]: rowError }));
    } finally {
      setPending(({ [workspace]: _, ...rest }) => rest);
      void refresh();
    }
  };

  return (
    <div className={`fleet${jobId ? " has-console" : ""}`}>
      <section className="register" aria-labelledby="register-title">
        <header className="screen-head">
          <div>
            <p className="eyebrow">Register of workspaces</p>
            <h1 id="register-title">Workspaces</h1>
          </div>
          <div className="screen-actions">
            <button type="button" className="ghost" onClick={reload} disabled={loading}>
              {loading ? "Refreshing…" : "Refresh"}
            </button>
            <Link className="button primary" href={paths.create()}>
              New workspace
            </Link>
          </div>
        </header>

        {error ? (
          <div className="notice notice-bad" role="alert">
            <p>
              <span className="mono">{error.kind}</span> {error.message}
            </p>
          </div>
        ) : null}

        {workspaces === null && !error ? (
          <p className="muted">Reading the workspace root…</p>
        ) : null}
        {workspaces !== null && workspaces.length === 0 ? (
          <div className="empty">
            <p>The workspace root has no folders yet.</p>
            <Link className="button primary" href={paths.create()}>
              Create the first workspace
            </Link>
          </div>
        ) : null}

        {workspaces && workspaces.length > 0 ? (
          <ol className="ledger" aria-label="Workspaces">
            {workspaces.map((ws, index) => {
              const active = ws.activeJob;
              const busy = active !== null || pending[ws.name] !== undefined;
              const rowError = rowErrors[ws.name];
              return (
                <li
                  key={ws.name}
                  className={`entry${busy ? " is-busy" : ""}`}
                  style={{ "--i": index } as CSSProperties}
                >
                  <span className="entry-no mono" aria-hidden="true">
                    {String(index + 1).padStart(3, "0")}
                  </span>
                  <div className="entry-name">
                    <h2 className="mono">{ws.name}</h2>
                    <p className="entry-container mono" title={ws.containerId ?? undefined}>
                      {ws.containerId ? `container ${ws.containerId.slice(0, 12)}` : "no container"}
                    </p>
                  </div>
                  <div className="entry-state">
                    <StateStamp state={ws.state} />
                    {active ? (
                      <Link className="busy-link" href={paths.list(active.jobId)}>
                        {operationLabel[active.operation] ?? active.operation} · {active.state}
                      </Link>
                    ) : null}
                  </div>
                  <div className="entry-actions" role="group" aria-label={`Actions for ${ws.name}`}>
                    <button
                      type="button"
                      disabled={busy || ws.state === "running"}
                      onClick={() => void run(ws.name, "start")}
                    >
                      Start
                    </button>
                    <button
                      type="button"
                      disabled={busy || ws.state !== "running"}
                      onClick={() => void run(ws.name, "stop")}
                    >
                      Stop
                    </button>
                    {confirming === ws.name ? (
                      <span className="confirm">
                        <button
                          type="button"
                          className="danger"
                          autoFocus
                          onClick={() => void run(ws.name, "rebuild")}
                        >
                          Confirm rebuild
                        </button>
                        <button type="button" className="ghost" onClick={() => setConfirming(null)}>
                          Keep
                        </button>
                      </span>
                    ) : (
                      <button type="button" disabled={busy} onClick={() => setConfirming(ws.name)}>
                        Rebuild
                      </button>
                    )}
                    {busy ? (
                      <span className="button disabled" aria-disabled="true">
                        Upgrade
                      </span>
                    ) : (
                      <Link className="button" href={paths.upgrade(ws.name)}>
                        Upgrade
                      </Link>
                    )}
                  </div>
                  {rowError ? (
                    <p className="entry-error" role="alert">
                      {rowError.message}
                      {rowError.activeJobId ? (
                        <>
                          {" "}
                          <Link href={paths.list(rowError.activeJobId)}>Show the running job</Link>
                        </>
                      ) : null}
                    </p>
                  ) : null}
                </li>
              );
            })}
          </ol>
        ) : null}

        <RecentJobs jobs={jobs} openJobId={jobId} />
      </section>

      {jobId ? (
        <aside className="console-dock" aria-label="Selected job">
          <div className="dock-close">
            <Link href={paths.list()} className="ghost button">
              Close
            </Link>
          </div>
          <JobPanel key={jobId} jobId={jobId} onFinished={() => void refresh()} />
        </aside>
      ) : null}
    </div>
  );
}

function RecentJobs({ jobs, openJobId }: { jobs: Job[]; openJobId: string | null }) {
  if (jobs.length === 0) return null;
  return (
    <section className="recent" aria-labelledby="recent-title">
      <h2 id="recent-title" className="eyebrow">
        Jobs since Menagerie started
      </h2>
      <ol>
        {jobs.map((job) => (
          <li key={job.jobId} className={job.jobId === openJobId ? "is-open" : undefined}>
            <Link href={paths.list(job.jobId)}>
              <span className="mono">{job.workspace}</span> {job.operation}
            </Link>
            <StateStamp state={job.state} />
            <time className="mono" dateTime={job.createdAt}>
              {new Date(job.createdAt).toLocaleTimeString()}
            </time>
          </li>
        ))}
      </ol>
    </section>
  );
}

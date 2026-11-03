import { useCallback, useEffect, useState } from "react";
import {
  ApiError,
  listJobs,
  listWorkspaces,
  mintT3CodePairingToken,
  startJob,
} from "../api/client.ts";
import type { Job, Operation, T3CodePairingToken, Workspace } from "../api/types.ts";
import { JobPanel } from "../jobs/JobPanel.tsx";
import { own } from "../own.ts";
import { navigate, paths } from "../router.ts";
import { Link } from "../ui/Link.tsx";
import { StatusBadge } from "../ui/StatusBadge.tsx";

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
  commit: "committing",
  "apply-branch": "applying a branch",
  delete: "deleting",
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
  const [applying, setApplying] = useState<string | null>(null);
  const [branch, setBranch] = useState("");
  const [deleting, setDeleting] = useState<string | null>(null);
  const [typedName, setTypedName] = useState("");
  const [minting, setMinting] = useState<Record<string, true>>({});
  const [tokens, setTokens] = useState<Record<string, T3CodePairingToken>>({});

  const mint = async (workspace: string) => {
    setMinting((m) => ({ ...m, [workspace]: true }));
    setRowErrors(({ [workspace]: _, ...rest }) => rest);
    setTokens(({ [workspace]: _, ...rest }) => rest);
    try {
      const token = await mintT3CodePairingToken(workspace);
      setTokens((t) => ({ ...t, [workspace]: token }));
    } catch (e) {
      const err = e instanceof ApiError ? e : new ApiError(0, null, String(e));
      setRowErrors((r) => ({ ...r, [workspace]: { message: err.message } }));
    } finally {
      setMinting(({ [workspace]: _, ...rest }) => rest);
    }
  };

  const openDelete = (workspace: string) => {
    setConfirming(null);
    setApplying(null);
    setTypedName("");
    setDeleting(workspace);
  };

  const openApply = (workspace: string) => {
    setConfirming(null);
    setDeleting(null);
    setBranch("");
    setApplying(workspace);
  };

  const run = async (workspace: string, operation: Operation, branchName?: string) => {
    setConfirming(null);
    setApplying(null);
    setDeleting(null);
    setPending((p) => ({ ...p, [workspace]: operation }));
    setRowErrors(({ [workspace]: _, ...rest }) => rest);
    try {
      const job = await startJob(
        branchName === undefined
          ? { operation, workspace }
          : { operation, workspace, branch: branchName },
      );
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
          <h1 id="register-title">Workspaces</h1>
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
            {workspaces.map((ws) => {
              const active = ws.activeJob;
              const busy = active !== null || own(pending, ws.name) !== undefined;
              const rowError = own(rowErrors, ws.name);
              const token = own(tokens, ws.name);
              const isMinting = own(minting, ws.name) !== undefined;
              return (
                <li key={ws.name} className={`entry${busy ? " is-busy" : ""}`}>
                  <div className="entry-name">
                    <h2 className="mono">{ws.name}</h2>
                    <p className="entry-container mono" title={ws.containerId ?? undefined}>
                      {ws.containerId ? `container ${ws.containerId.slice(0, 12)}` : "no container"}
                    </p>
                  </div>
                  <div className="entry-state">
                    <StatusBadge state={ws.state} />
                    {active ? (
                      <Link className="busy-link" href={paths.list(active.jobId)}>
                        {own(operationLabel, active.operation) ?? active.operation} · {active.state}
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
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => {
                          setApplying(null);
                          setDeleting(null);
                          setConfirming(ws.name);
                        }}
                      >
                        Rebuild
                      </button>
                    )}
                    {applying === ws.name ? (
                      <form
                        className="apply-branch"
                        aria-label={`Apply a branch to ${ws.name}`}
                        onSubmit={(e) => {
                          e.preventDefault();
                          void run(ws.name, "apply-branch", branch.trim());
                        }}
                      >
                        <input
                          type="text"
                          className="mono"
                          name="branch"
                          aria-label="Branch"
                          placeholder="branch"
                          autoComplete="off"
                          spellCheck={false}
                          autoFocus
                          value={branch}
                          onChange={(e) => setBranch(e.target.value)}
                        />
                        <button type="submit" className="primary" disabled={busy || !branch.trim()}>
                          Apply
                        </button>
                        <button type="button" className="ghost" onClick={() => setApplying(null)}>
                          Cancel
                        </button>
                      </form>
                    ) : (
                      <button type="button" disabled={busy} onClick={() => openApply(ws.name)}>
                        Apply branch
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
                    <button
                      type="button"
                      disabled={isMinting || ws.state !== "running"}
                      onClick={() => void mint(ws.name)}
                    >
                      {isMinting ? "Minting…" : "T3 Code token"}
                    </button>
                    <button
                      type="button"
                      disabled={busy || deleting === ws.name}
                      onClick={() => openDelete(ws.name)}
                    >
                      Delete
                    </button>
                  </div>
                  {deleting === ws.name ? (
                    <form
                      className="entry-delete"
                      aria-label={`Delete ${ws.name}`}
                      onSubmit={(e) => {
                        e.preventDefault();
                        if (typedName === ws.name) void run(ws.name, "delete");
                      }}
                    >
                      <p>
                        Delete removes the container and the folder of{" "}
                        <span className="mono">{ws.name}</span>. Tracked and new files are backed up
                        first. Ignored files, such as cloned repositories, are not backed up and are
                        lost.
                      </p>
                      <input
                        type="text"
                        className="mono"
                        name="workspace-name"
                        aria-label="Type the workspace's name"
                        placeholder={ws.name}
                        autoComplete="off"
                        spellCheck={false}
                        autoFocus
                        value={typedName}
                        onChange={(e) => setTypedName(e.target.value)}
                      />
                      <span className="entry-delete-actions">
                        <button
                          type="submit"
                          className="danger"
                          disabled={busy || typedName !== ws.name}
                        >
                          Delete workspace
                        </button>
                        <button type="button" className="ghost" onClick={() => setDeleting(null)}>
                          Keep
                        </button>
                      </span>
                    </form>
                  ) : null}
                  {token ? (
                    <PairingToken
                      token={token}
                      onDismiss={() => setTokens(({ [ws.name]: _, ...rest }) => rest)}
                    />
                  ) : null}
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

function PairingToken({ token, onDismiss }: { token: T3CodePairingToken; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false);
  // The clipboard needs a secure context. Without one the token stays selectable.
  const copy = () => {
    void navigator.clipboard?.writeText(token.token).then(
      () => setCopied(true),
      () => undefined,
    );
  };
  return (
    <div className="entry-token" role="group" aria-label="T3 Code pairing token">
      <p>
        Pair a T3 Code client with this token before{" "}
        <time dateTime={token.expiresAt}>{new Date(token.expiresAt).toLocaleTimeString()}</time>. It
        is shown once.
      </p>
      <code className="mono">{token.token}</code>
      <span className="entry-token-actions">
        <button type="button" onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </button>
        <button type="button" className="ghost" onClick={onDismiss}>
          Dismiss
        </button>
      </span>
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
            <StatusBadge state={job.state} />
            <time className="mono" dateTime={job.createdAt}>
              {new Date(job.createdAt).toLocaleTimeString()}
            </time>
          </li>
        ))}
      </ol>
    </section>
  );
}

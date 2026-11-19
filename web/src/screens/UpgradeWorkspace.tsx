import { useEffect, useState, type FormEvent } from "react";
import { ApiError, listTemplates, listWorkspaces, startJob } from "../api/client.ts";
import type { Template, Workspace } from "../api/types.ts";
import { JobPanel } from "../jobs/JobPanel.tsx";
import { navigate, paths } from "../router.ts";
import { Link } from "../ui/Link.tsx";

export function UpgradeWorkspace({
  workspace,
  jobId,
}: {
  workspace: string;
  jobId: string | null;
}) {
  return (
    <section className="screen upgrade" aria-labelledby="upgrade-title">
      <header className="screen-head">
        <div>
          <p className="eyebrow">
            <Link href={paths.list()}>Workspaces</Link> / <span className="mono">{workspace}</span>{" "}
            / upgrade
          </p>
          <h1 id="upgrade-title">
            Upgrade <span className="mono">{workspace}</span>
          </h1>
        </div>
      </header>
      {jobId ? (
        <>
          <JobPanel key={jobId} jobId={jobId} heading="Upgrade job" />
          <p className="after-job">
            <Link href={paths.list()}>Back to the workspace list</Link>
          </p>
        </>
      ) : (
        <StartUpgrade workspace={workspace} />
      )}
    </section>
  );
}

function StartUpgrade({ workspace }: { workspace: string }) {
  const [current, setCurrent] = useState<Workspace | null | undefined>(undefined);
  const [template, setTemplate] = useState<Template | null>(null);
  const [reanswer, setReanswer] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [starting, setStarting] = useState(false);

  useEffect(() => {
    Promise.all([listWorkspaces(), listTemplates()])
      .then(([all, templates]) => {
        const found = all.find((w) => w.name === workspace) ?? null;
        setCurrent(found);
        setTemplate(templates.find((t) => t.id === found?.template?.id) ?? null);
      })
      .catch((e: unknown) =>
        setError(e instanceof ApiError ? e : new ApiError(0, null, String(e))),
      );
  }, [workspace]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setStarting(true);
    setError(null);
    try {
      // The upgrade applies the current version of the workspace's own template.
      const job = await startJob({
        operation: "upgrade",
        workspace,
        ...(reanswer ? { reanswer } : {}),
      });
      navigate(paths.upgrade(workspace, job.jobId), { replace: true });
    } catch (e) {
      setError(e instanceof ApiError ? e : new ApiError(0, null, String(e)));
      setStarting(false);
    }
  };

  if (current === null) {
    return (
      <div className="notice notice-bad" role="alert">
        <p>
          No folder named <span className="mono">{workspace}</span> is under the workspace root.
        </p>
        <Link href={paths.list()}>Back to the workspace list</Link>
      </div>
    );
  }

  const active = current?.activeJob ?? null;
  const unregistered = current !== undefined && current.template === null;
  const moved = template?.currentMoved === true;
  // A known applied version that is the current one, or newer, needs nothing.
  const upToDate =
    current?.template != null &&
    template !== null &&
    current.appliedVersion !== null &&
    !current.needsUpgrade;
  return (
    <form className="start-form" noValidate onSubmit={(e) => void submit(e)}>
      {current ? (
        <div className="upgrade-current">
          <span>Applied</span>
          <strong
            title={
              current.appliedVersion === null && current.appliedCommit
                ? `Applied commit ${current.appliedCommit}`
                : undefined
            }
          >
            {current.appliedVersion ?? "Unknown version"}
          </strong>
          <span>Current template</span>
          <strong>
            {current.template ? current.template.name : "Unregistered"}
            {template ? ` ${template.currentVersion}` : null}
          </strong>
        </div>
      ) : (
        <p className="muted">Reading…</p>
      )}
      {unregistered ? (
        <p className="field-errors" role="alert">
          Register this workspace's template before upgrading.
        </p>
      ) : moved ? (
        <p className="field-errors" role="alert">
          The current tag moved. Validate and save the version on Templates before upgrading.
        </p>
      ) : upToDate ? (
        <p className="muted">This workspace already has the current version.</p>
      ) : null}
      {active ? (
        <div className="notice notice-warn" role="status">
          <p>
            This workspace already has a {active.operation} job ({active.state}). One job runs per
            workspace.
          </p>
          <Link href={paths.list(active.jobId)}>Show the running job</Link>
        </div>
      ) : null}
      <label className="choice">
        <input type="checkbox" checked={reanswer} onChange={(e) => setReanswer(e.target.checked)} />
        <span>Ask every question again</span>
      </label>
      {error ? (
        <div className="notice notice-bad" role="alert">
          <p>
            <span className="mono">{error.kind}</span> {error.message}
          </p>
          {error.body?.activeJobId ? (
            <Link href={paths.list(error.body.activeJobId)}>Show the running job</Link>
          ) : null}
        </div>
      ) : null}
      <div className="interview-actions">
        <button
          type="submit"
          className="primary"
          disabled={starting || active !== null || unregistered || moved || upToDate}
        >
          {starting ? "Starting…" : "Begin upgrade"}
        </button>
      </div>
    </form>
  );
}

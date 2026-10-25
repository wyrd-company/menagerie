import { useEffect, useState, type FormEvent } from "react";
import { ApiError, listWorkspaces, startJob } from "../api/client.ts";
import type { Workspace } from "../api/types.ts";
import { JobPanel } from "../jobs/JobPanel.tsx";
import { navigate, paths } from "../router.ts";
import { Link } from "../ui/Link.tsx";
import { StatusBadge } from "../ui/StatusBadge.tsx";
import { TemplatePicker } from "../ui/TemplatePicker.tsx";

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
  const [templateId, setTemplateId] = useState<string | null>(null);
  const [checked, setChecked] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [starting, setStarting] = useState(false);

  useEffect(() => {
    listWorkspaces()
      .then((all) => setCurrent(all.find((w) => w.name === workspace) ?? null))
      .catch((e: unknown) =>
        setError(e instanceof ApiError ? e : new ApiError(0, null, String(e))),
      );
  }, [workspace]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setChecked(true);
    if (templateId === null) return;
    setStarting(true);
    setError(null);
    try {
      const job = await startJob({ operation: "upgrade", workspace, templateId });
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
  return (
    <form className="start-form" noValidate onSubmit={(e) => void submit(e)}>
      <div className="upgrade-current">
        {current ? <StatusBadge state={current.state} /> : <span className="muted">Reading…</span>}
        <p>
          Menagerie commits and backs up local changes to tracked files first. The interview then
          asks only the questions that the recorded answers in{" "}
          <span className="mono">.toha.json</span> do not cover, and the workspace is rebuilt after
          the merge.
        </p>
      </div>
      {active ? (
        <div className="notice notice-warn" role="status">
          <p>
            This workspace already has a {active.operation} job ({active.state}). One job runs per
            workspace.
          </p>
          <Link href={paths.list(active.jobId)}>Show the running job</Link>
        </div>
      ) : null}
      <TemplatePicker value={templateId} onChange={setTemplateId} legend="Upgrade to template" />
      {checked && templateId === null ? (
        <p className="field-errors" role="alert">
          Choose a template.
        </p>
      ) : null}
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
        <button type="submit" className="primary" disabled={starting || active !== null}>
          {starting ? "Starting…" : "Begin upgrade"}
        </button>
      </div>
    </form>
  );
}

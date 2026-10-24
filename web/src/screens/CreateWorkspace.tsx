import { useState, type FormEvent } from "react";
import { ApiError, startJob } from "../api/client.ts";
import { JobPanel } from "../jobs/JobPanel.tsx";
import { navigate, paths } from "../router.ts";
import { Link } from "../ui/Link.tsx";
import { TemplatePicker } from "../ui/TemplatePicker.tsx";

/** Problems the browser can see in a folder name; the service checks the rest. */
export function nameProblems(name: string): string[] {
  if (name === "") return ["is required"];
  if (name.includes("/") || name.includes("\\"))
    return ["must be one folder name, without slashes"];
  if (name === "." || name === "..") return ["must not be . or .."];
  return [];
}

export function CreateWorkspace({ jobId }: { jobId: string | null }) {
  return (
    <section className="screen create" aria-labelledby="create-title">
      <header className="screen-head">
        <div>
          <p className="eyebrow">
            <Link href={paths.list()}>Workspaces</Link> / new
          </p>
          <h1 id="create-title">New workspace</h1>
        </div>
      </header>
      {jobId ? (
        <>
          <JobPanel key={jobId} jobId={jobId} heading="Create job" />
          <p className="after-job">
            <Link href={paths.list()}>Back to the workspace list</Link>
          </p>
        </>
      ) : (
        <StartCreate />
      )}
    </section>
  );
}

function StartCreate() {
  const [templateId, setTemplateId] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [checked, setChecked] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [starting, setStarting] = useState(false);

  const problems = nameProblems(name.trim());
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setChecked(true);
    if (problems.length > 0 || templateId === null) return;
    setStarting(true);
    setError(null);
    try {
      const job = await startJob({ operation: "create", workspace: name.trim(), templateId });
      navigate(paths.create(job.jobId), { replace: true });
    } catch (e) {
      setError(e instanceof ApiError ? e : new ApiError(0, null, String(e)));
      setStarting(false);
    }
  };

  const nameErrors = checked ? problems : [];
  return (
    <form className="start-form" noValidate onSubmit={(e) => void submit(e)}>
      <TemplatePicker value={templateId} onChange={setTemplateId} legend="Template" />
      {checked && templateId === null ? (
        <p className="field-errors" role="alert">
          Choose a template.
        </p>
      ) : null}
      <div className={`field${nameErrors.length ? " is-invalid" : ""}`}>
        <label className="field-label" htmlFor="workspace-name">
          <span className="field-title">Workspace name</span>
          <span className="required" aria-label="required">
            ✱
          </span>
        </label>
        <p className="field-description" id="workspace-name-desc">
          The folder under the workspace root, and the name of its backup branch.
        </p>
        <input
          id="workspace-name"
          className="mono"
          autoComplete="off"
          spellCheck={false}
          placeholder="sample-space"
          value={name}
          aria-describedby="workspace-name-desc"
          aria-invalid={nameErrors.length > 0 || undefined}
          onChange={(e) => setName(e.target.value)}
        />
        {nameErrors.length ? (
          <ul className="field-errors">
            {nameErrors.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        ) : null}
      </div>
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
        <button type="submit" className="primary" disabled={starting}>
          {starting ? "Starting…" : "Begin interview"}
        </button>
      </div>
    </form>
  );
}

import { useState, useEffect, type FormEvent } from "react";
import { ApiError, listPolicies, startJob } from "../api/client.ts";
import type { Policies } from "../api/types.ts";
import { PolicyChoices, choiceProblems } from "../ui/PolicyChoices.tsx";
import { JobPanel } from "../jobs/JobPanel.tsx";
import { navigate, paths } from "../router.ts";
import { Link } from "../ui/Link.tsx";
import { TemplatePicker } from "../ui/TemplatePicker.tsx";

/** Problems the browser can see in a folder name; the service checks the rest. */
export function nameProblems(name: string): string[] {
  if (name === "") return ["is required"];
  if (name.includes("/") || name.includes("\\"))
    return ["must be one folder name, without slashes"];
  if (name.startsWith(".")) return ["must not start with a dot"];
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

  const [catalog, setCatalog] = useState<Policies | null>(null);
  const [policies, setPolicies] = useState<string[]>([]);
  const [policyError, setPolicyError] = useState("");
  const [policyAttempt, setPolicyAttempt] = useState(0);
  useEffect(() => {
    let live = true;
    void listPolicies()
      .then((value) => {
        if (live) {
          setCatalog(value);
          setPolicies((previous) => [...new Set([...previous, ...value.settings.required])]);
          setPolicyError("");
        }
      })
      .catch((e: unknown) => {
        if (live) setPolicyError(String(e));
      });
    return () => {
      live = false;
    };
  }, [policyAttempt]);
  const problems = nameProblems(name.trim());
  const policyProblems = catalog ? choiceProblems(catalog, policies) : [];
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setChecked(true);
    if (problems.length > 0 || templateId === null || !catalog || policyProblems.length) return;
    setStarting(true);
    setError(null);
    try {
      const job = await startJob({
        operation: "create",
        workspace: name.trim(),
        templateId,
        policies,
      });
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
      <fieldset className="create-policies">
        <legend>Policies</legend>
        {catalog ? (
          <PolicyChoices
            catalog={catalog}
            selected={policies}
            onChange={setPolicies}
            disabled={starting || !catalog.grant}
          />
        ) : policyError ? (
          <div role="alert">
            <p>{policyError}</p>
            <button type="button" onClick={() => setPolicyAttempt((n) => n + 1)}>
              Retry policies
            </button>
          </div>
        ) : (
          <p role="status">Loading policies…</p>
        )}
        {checked && policyProblems.length ? (
          <ul className="field-errors" role="alert">
            {policyProblems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        ) : null}
      </fieldset>
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
        <button type="submit" className="primary" disabled={starting || !catalog}>
          {starting ? "Starting…" : "Begin interview"}
        </button>
      </div>
    </form>
  );
}

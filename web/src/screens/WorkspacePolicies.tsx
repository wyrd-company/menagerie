import { useEffect, useState, type FormEvent } from "react";
import { listPolicies, listWorkspaces, saveWorkspacePolicies, startJob } from "../api/client.ts";
import type { Policies, Workspace } from "../api/types.ts";
import { navigate, paths } from "../router.ts";
import { PolicyChoices, choiceProblems } from "../ui/PolicyChoices.tsx";

export function policyStatus(ws: Workspace): string {
  if (!ws.selectionImported) return "Policy import pending";
  if (ws.applyFailure) return "Policies failed";
  return ws.appliedRevision === ws.selectionRevision ? "Policies applied" : "Policies pending";
}

export function WorkspacePolicies({
  workspace,
  onSaved,
}: {
  workspace: Workspace;
  onSaved: () => Promise<void>;
}) {
  const [catalog, setCatalog] = useState<Policies | null>(null);
  const [selected, setSelected] = useState(workspace.policies);
  const [revision, setRevision] = useState(workspace.selectionRevision);
  const [previous, setPrevious] = useState(workspace.policies);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState("");
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let live = true;
    void listPolicies()
      .then((value) => {
        if (live) {
          setCatalog(value);
          setError("");
        }
      })
      .catch((e: unknown) => {
        if (live) setError(String(e));
      });
    return () => {
      live = false;
    };
  }, [attempt]);
  const reload = async () => {
    setSaving(true);
    try {
      const [policies, rows] = await Promise.all([listPolicies(), listWorkspaces()]);
      const current = rows.find((w) => w.name === workspace.name);
      if (!current) throw new Error("The workspace was deleted. Refresh the workspace list.");
      setCatalog(policies);
      setSelected(current.policies);
      setPrevious(current.policies);
      setRevision(current.selectionRevision);
      setError("");
      setSaved("");
      await onSaved();
    } catch (e) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };
  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (!catalog) return;
    const problems = choiceProblems(catalog, selected, previous);
    if (problems.length) {
      setError(problems.join(" "));
      return;
    }
    setSaving(true);
    setError("");
    setSaved("");
    try {
      const result = await saveWorkspacePolicies(workspace.name, selected, revision);
      setRevision(result.selectionRevision);
      setPrevious(selected);
      setSaved(
        result.jobId ? "Saved. Applying policies…" : "Saved. Policies apply after the running job.",
      );
      await onSaved();
      if (result.jobId) navigate(paths.list(result.jobId));
    } catch (e) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };
  const retry = async () => {
    setSaving(true);
    setError("");
    try {
      const job = await startJob({ operation: "apply-policies", workspace: workspace.name });
      navigate(paths.list(job.jobId));
    } catch (e) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };
  const dirty = selected.length !== previous.length || selected.some((p) => !previous.includes(p));
  return (
    <form
      className="workspace-policy-panel"
      aria-label={`Policies for ${workspace.name}`}
      onSubmit={(event) => void save(event)}
    >
      <div className="policy-actions">
        <h3>{workspace.name} policies</h3>
        <span>{policyStatus(workspace)}</span>
      </div>
      {catalog ? (
        <PolicyChoices
          catalog={catalog}
          selected={selected}
          onChange={(value) => {
            setSelected(value);
            setSaved("");
          }}
          disabled={saving || !catalog.grant}
        />
      ) : (
        <p role="status">Loading policies…</p>
      )}
      {workspace.xolotlKey ? (
        <p className="muted">
          Xolotl key: <span className="mono">{workspace.xolotlKey}</span>
        </p>
      ) : null}
      {workspace.applyFailure ? (
        <p className="field-errors">{workspace.applyFailure.message}</p>
      ) : null}
      {error ? (
        <div role="alert" className="field-errors">
          <p>{error}</p>
          <button type="button" disabled={saving} onClick={() => void reload()}>
            Reload policies
          </button>
          {!catalog ? (
            <button type="button" onClick={() => setAttempt((n) => n + 1)}>
              Retry
            </button>
          ) : null}
        </div>
      ) : null}
      <div className="policy-actions">
        <button className="primary" type="submit" disabled={saving || !catalog?.grant}>
          {saving ? "Saving…" : "Save"}
        </button>
        {workspace.appliedRevision !== workspace.selectionRevision || workspace.applyFailure ? (
          <button
            type="button"
            disabled={saving || !!workspace.activeJob || !catalog?.grant}
            onClick={() => void retry()}
          >
            Apply again
          </button>
        ) : null}
        <span className="muted" role="status">
          {saved || (dirty ? "Unsaved changes" : "")}
        </span>
      </div>
    </form>
  );
}

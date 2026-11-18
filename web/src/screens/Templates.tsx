import { useEffect, useState, type FormEvent } from "react";
import {
  ApiError,
  editTemplate,
  listTemplates,
  registerTemplate,
  removeTemplate,
  upgradeTemplate,
} from "../api/client.ts";
import type { Template } from "../api/types.ts";

const asError = (e: unknown) => (e instanceof ApiError ? e : new ApiError(0, null, String(e)));

/** The refusal's message, with the registration check it names. */
function refusal(error: ApiError): string {
  const check = error.body?.check;
  return check ? `${check}: ${error.message}` : error.message;
}

export function TemplatesScreen() {
  const [templates, setTemplates] = useState<Template[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [registering, setRegistering] = useState(false);

  const load = () =>
    listTemplates()
      .then((list) => {
        setTemplates(list);
        setError(null);
      })
      .catch((e: unknown) => setError(asError(e)));
  useEffect(() => {
    void load();
  }, []);

  const replace = (next: Template) =>
    setTemplates((list) => (list ?? []).map((t) => (t.id === next.id ? next : t)));
  const open = () => setRegistering(true);

  return (
    <section className="screen templates" aria-labelledby="templates-title">
      <header className="screen-head">
        <h1 id="templates-title">Templates</h1>
        <button type="button" className="primary" onClick={open} aria-expanded={registering}>
          Register template
        </button>
      </header>
      {registering ? (
        <Registration
          onRegistered={() => {
            setRegistering(false);
            void load();
          }}
        />
      ) : null}
      {error ? (
        <div className="notice notice-bad" role="alert">
          <p>{error.message}</p>
          <button type="button" onClick={() => void load()}>
            Retry
          </button>
        </div>
      ) : null}
      {templates === null && !error ? <p className="muted">Reading templates…</p> : null}
      {templates?.length === 0 ? (
        <div className="empty">
          <p>No registered templates.</p>
          {registering ? null : (
            <button type="button" className="primary" onClick={open}>
              Register template
            </button>
          )}
        </div>
      ) : null}
      {templates?.length ? (
        <ul className="ledger template-ledger">
          {templates.map((t) => (
            <TemplateEntry
              key={t.id}
              template={t}
              onChanged={replace}
              onRemoved={() => setTemplates((list) => (list ?? []).filter((x) => x.id !== t.id))}
            />
          ))}
        </ul>
      ) : null}
    </section>
  );
}

/**
 * Validate, then save: Validate runs the checks alone and stores nothing; Save
 * stays disabled until a Validate of the current values passes, and checks
 * again. Any edit disables Save and clears the result and refusal.
 */
function useValidated() {
  const [error, setError] = useState<ApiError | null>(null);
  const [validated, setValidated] = useState<Template | null>(null);
  const [busy, setBusy] = useState<"validating" | "saving" | null>(null);
  const edited = () => {
    setValidated(null);
    setError(null);
  };
  const run = async (kind: "validating" | "saving", call: () => Promise<Template>) => {
    setBusy(kind);
    setError(null);
    try {
      const template = await call();
      if (kind === "validating") setValidated(template);
      return template;
    } catch (e) {
      setError(asError(e));
      setValidated(null);
      return null;
    } finally {
      setBusy(null);
    }
  };
  return { error, validated, busy, edited, run };
}

function Registration({ onRegistered }: { onRegistered: () => void }) {
  const [name, setName] = useState("");
  const [address, setAddress] = useState("");
  const [runnerImage, setRunnerImage] = useState("");
  const { error, validated, busy, edited, run } = useValidated();
  const registration = (check: boolean) => ({
    name: name.trim(),
    address: address.trim(),
    ...(runnerImage.trim() ? { runnerImage: runnerImage.trim() } : {}),
    ...(check ? { check } : {}),
  });
  const filled = name.trim() !== "" && address.trim() !== "";

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!validated) return;
    if (await run("saving", () => registerTemplate(registration(false)))) onRegistered();
  };

  return (
    <form
      className="template-panel"
      aria-labelledby="register-title"
      onSubmit={(e) => void submit(e)}
    >
      <h2 id="register-title">Register template</h2>
      <div className="field">
        <label className="field-label" htmlFor="template-name">
          <span className="field-title">Display name</span>
        </label>
        <input
          id="template-name"
          required
          maxLength={100}
          autoComplete="off"
          autoFocus
          value={name}
          onChange={(e) => {
            setName(e.target.value);
            edited();
          }}
        />
      </div>
      <div className="field">
        <label className="field-label" htmlFor="template-address">
          <span className="field-title">Address</span>
        </label>
        <input
          id="template-address"
          className="mono"
          required
          autoComplete="off"
          spellCheck={false}
          value={address}
          aria-describedby="registration-error"
          aria-invalid={error?.body?.check === "address" ? true : undefined}
          onChange={(e) => {
            setAddress(e.target.value);
            edited();
          }}
        />
      </div>
      <div className="field">
        <label className="field-label" htmlFor="runner-image">
          <span className="field-title">Runner image</span>
          <span className="muted">(optional)</span>
        </label>
        <input
          id="runner-image"
          className="mono"
          autoComplete="off"
          spellCheck={false}
          placeholder="Use host default"
          value={runnerImage}
          onChange={(e) => {
            setRunnerImage(e.target.value);
            edited();
          }}
        />
      </div>
      <p className="form-error" id="registration-error" role="alert">
        {error ? refusal(error) : null}
      </p>
      <p className="form-success" id="registration-result" role="status">
        {validated ? `Checks passed · ${validated.currentVersion}` : null}
      </p>
      <div className="form-actions">
        <button
          type="button"
          disabled={busy !== null || !filled}
          onClick={() => void run("validating", () => registerTemplate(registration(true)))}
        >
          {busy === "validating" ? "Checking…" : "Validate"}
        </button>
        <button type="submit" className="primary" disabled={busy !== null || !validated}>
          {busy === "saving" ? "Saving…" : "Save"}
        </button>
      </div>
    </form>
  );
}

type Editing = "upgrade" | "edit" | null;

function TemplateEntry({
  template: t,
  onChanged,
  onRemoved,
}: {
  template: Template;
  onChanged: (t: Template) => void;
  onRemoved: () => void;
}) {
  const [editing, setEditing] = useState<Editing>(null);
  const [removeError, setRemoveError] = useState<ApiError | null>(null);
  const [removing, setRemoving] = useState(false);
  const toggle = (next: Editing) => setEditing((current) => (current === next ? null : next));

  const remove = async () => {
    setRemoving(true);
    setRemoveError(null);
    try {
      await removeTemplate(t.id);
      onRemoved();
    } catch (e) {
      setRemoveError(asError(e));
      setRemoving(false);
    }
  };

  return (
    <li className="template-entry" aria-labelledby={`template-${t.id}`}>
      <div>
        <h2 id={`template-${t.id}`}>{t.name}</h2>
        <p className="muted mono template-address">{t.address}</p>
        {removeError ? (
          <p className="form-error" role="alert">
            {removeError.message}
          </p>
        ) : null}
      </div>
      <div className="template-versions">
        <span>
          Current <strong>{t.currentVersion}</strong>
        </span>
      </div>
      <div className="template-actions" role="group" aria-label={`Actions for ${t.name}`}>
        <button
          type="button"
          aria-expanded={editing === "upgrade"}
          onClick={() => toggle("upgrade")}
        >
          Upgrade template
        </button>
        <button type="button" aria-expanded={editing === "edit"} onClick={() => toggle("edit")}>
          Edit
        </button>
        <button type="button" disabled={removing} onClick={() => void remove()}>
          Remove
        </button>
      </div>
      {editing === "upgrade" ? (
        <VersionEditor
          template={t}
          onSaved={(next) => {
            onChanged(next);
            setEditing(null);
          }}
        />
      ) : null}
      {editing === "edit" ? (
        <DetailsEditor
          template={t}
          onSaved={(next) => {
            onChanged(next);
            setEditing(null);
          }}
        />
      ) : null}
    </li>
  );
}

function VersionEditor({
  template: t,
  onSaved,
}: {
  template: Template;
  onSaved: (t: Template) => void;
}) {
  const [version, setVersion] = useState("");
  const { error, validated, busy, edited, run } = useValidated();
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!validated) return;
    const saved = await run("saving", () => upgradeTemplate(t.id, version.trim()));
    if (saved) onSaved(saved);
  };
  return (
    <form className="template-edit" onSubmit={(e) => void submit(e)}>
      <div className="field">
        <label className="field-label" htmlFor={`version-${t.id}`}>
          <span className="field-title">Current version</span>
        </label>
        <input
          id={`version-${t.id}`}
          className="mono"
          required
          autoComplete="off"
          spellCheck={false}
          autoFocus
          placeholder={`newer than ${t.currentVersion}`}
          value={version}
          aria-describedby={`version-error-${t.id}`}
          onChange={(e) => {
            setVersion(e.target.value);
            edited();
          }}
        />
      </div>
      <p className="form-error" id={`version-error-${t.id}`} role="alert">
        {error ? refusal(error) : null}
      </p>
      <p className="form-success" id={`version-result-${t.id}`} role="status">
        {validated ? `Checks passed · ${validated.currentVersion}` : null}
      </p>
      <div className="form-actions">
        <button
          type="button"
          disabled={busy !== null || !version.trim()}
          onClick={() => void run("validating", () => upgradeTemplate(t.id, version.trim(), true))}
        >
          {busy === "validating" ? "Checking…" : "Validate"}
        </button>
        <button type="submit" className="primary" disabled={busy !== null || !validated}>
          {busy === "saving" ? "Saving…" : "Save"}
        </button>
      </div>
    </form>
  );
}

function DetailsEditor({
  template: t,
  onSaved,
}: {
  template: Template;
  onSaved: (t: Template) => void;
}) {
  const [name, setName] = useState(t.name);
  const [runnerImage, setRunnerImage] = useState(t.runnerImage ?? "");
  const [error, setError] = useState<ApiError | null>(null);
  const [saving, setSaving] = useState(false);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      onSaved(
        await editTemplate(t.id, {
          name: name.trim(),
          runnerImage: runnerImage.trim() ? runnerImage.trim() : null,
        }),
      );
    } catch (e) {
      setError(asError(e));
      setSaving(false);
    }
  };
  return (
    <form className="template-edit" onSubmit={(e) => void submit(e)}>
      <div className="field">
        <label className="field-label" htmlFor={`edit-name-${t.id}`}>
          <span className="field-title">Display name</span>
        </label>
        <input
          id={`edit-name-${t.id}`}
          required
          maxLength={100}
          autoComplete="off"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      <div className="field">
        <label className="field-label" htmlFor={`edit-runner-${t.id}`}>
          <span className="field-title">Runner image</span>
          <span className="muted">(optional)</span>
        </label>
        <input
          id={`edit-runner-${t.id}`}
          className="mono"
          autoComplete="off"
          spellCheck={false}
          placeholder="Use host default"
          value={runnerImage}
          onChange={(e) => setRunnerImage(e.target.value)}
        />
      </div>
      <p className="form-error" role="alert">
        {error ? error.message : null}
      </p>
      <div className="form-actions">
        <button type="submit" className="primary" disabled={saving || !name.trim()}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
    </form>
  );
}

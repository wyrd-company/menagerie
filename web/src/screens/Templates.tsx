import { useEffect, useState, type FormEvent } from "react";
import {
  ApiError,
  checkTemplateVersions,
  editTemplate,
  listTemplates,
  registerTemplate,
  removeTemplate,
  upgradeTemplate,
} from "../api/client.ts";
import type { Template } from "../api/types.ts";
import { checkLine, hasNewer, upgradeChoices } from "../api/versions.ts";
import { StatusBadge } from "../ui/StatusBadge.tsx";
import { useValidated } from "../ui/useValidated.ts";

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

function Registration({ onRegistered }: { onRegistered: () => void }) {
  const [name, setName] = useState("");
  const [address, setAddress] = useState("");
  const [runnerImage, setRunnerImage] = useState("");
  const registration = (check: boolean) => ({
    name: name.trim(),
    address: address.trim(),
    ...(runnerImage.trim() ? { runnerImage: runnerImage.trim() } : {}),
    ...(check ? { check } : {}),
  });
  const { error, validated, busy, edited, run } = useValidated<Template>(
    JSON.stringify(registration(false)),
  );
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
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<ApiError | null>(null);
  const toggle = (next: Editing) => setEditing((current) => (current === next ? null : next));
  const newer = hasNewer(t);
  // Upgrade template while a newer version is found; Update version while
  // the current version's tag has moved.
  const upgradable = newer || t.currentMoved;

  const check = async () => {
    setChecking(true);
    setCheckError(null);
    try {
      onChanged(await checkTemplateVersions(t.id));
    } catch (e) {
      setCheckError(asError(e));
    } finally {
      setChecking(false);
    }
  };

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
        <p
          className={`template-check ${t.lastCheck?.failure && !checking ? "form-error" : "muted"}`}
          title={t.lastCheck ? new Date(t.lastCheck.at).toLocaleString() : undefined}
        >
          {checking ? "Checking versions…" : checkLine(t)}
        </p>
        {t.currentMoved ? (
          <p className="form-error template-moved">
            Current tag moved. Validate and save this version again before using it.
          </p>
        ) : null}
        {checkError ? (
          <p className="form-error" role="alert">
            {checkError.message}
          </p>
        ) : null}
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
        <span>
          Newest <strong>{t.newestVersion ?? "none found"}</strong>
        </span>
        {newer ? (
          <StatusBadge state="warn" label="New version" />
        ) : t.currentMoved ? (
          <StatusBadge state="failed" label="Tag moved" />
        ) : null}
      </div>
      <div className="template-actions" role="group" aria-label={`Actions for ${t.name}`}>
        <button type="button" disabled={checking} onClick={() => void check()}>
          Check versions
        </button>
        {upgradable ? (
          <button
            type="button"
            aria-expanded={editing === "upgrade"}
            onClick={() => toggle("upgrade")}
          >
            {t.currentMoved ? "Update version" : "Upgrade template"}
          </button>
        ) : null}
        <button type="button" aria-expanded={editing === "edit"} onClick={() => toggle("edit")}>
          Edit
        </button>
        <button type="button" disabled={removing} onClick={() => void remove()}>
          Remove
        </button>
      </div>
      {editing === "upgrade" && upgradable ? (
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

/**
 * Takes a found version newer than the template's current one, or the
 * current one again while its tag has moved: a select of those versions,
 * newest first, then Validate and Save.
 */
function VersionEditor({
  template: t,
  onSaved,
}: {
  template: Template;
  onSaved: (t: Template) => void;
}) {
  const choices = upgradeChoices(t);
  const [picked, setPicked] = useState(choices[0] ?? "");
  // A check while the form is open can take the picked version off the
  // offer; the newest offered one takes its place.
  const version = choices.includes(picked) ? picked : (choices[0] ?? "");
  const { error, validated, busy, edited, run } = useValidated<Template>(version);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!validated) return;
    const saved = await run("saving", () => upgradeTemplate(t.id, version));
    if (saved) onSaved(saved);
  };
  return (
    <form className="template-edit" onSubmit={(e) => void submit(e)}>
      <div className="field">
        <label className="field-label" htmlFor={`version-${t.id}`}>
          <span className="field-title">Current version</span>
        </label>
        <select
          id={`version-${t.id}`}
          className="mono"
          required
          autoFocus
          value={version}
          aria-describedby={`version-error-${t.id}`}
          onChange={(e) => {
            setPicked(e.target.value);
            edited();
          }}
        >
          {choices.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
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
          disabled={busy !== null || !version}
          onClick={() => void run("validating", () => upgradeTemplate(t.id, version, true))}
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

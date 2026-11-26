import { useEffect, useState, type FormEvent } from "react";
import {
  ApiError,
  listBackupRepositories,
  listCredentials,
  registerBackupRepository,
  removeBackupRepository,
  setBackupRepositoryCredential,
} from "../api/client.ts";
import type { BackupCredentialChoice, BackupRepository, Credentials } from "../api/types.ts";
import { CredentialPicker, describeCredential, effectiveChoice } from "../ui/CredentialPicker.tsx";
import { useValidated } from "../ui/useValidated.ts";

const asError = (e: unknown) => (e instanceof ApiError ? e : new ApiError(0, null, String(e)));

/** The refusal's message, with the registration check it names. */
function refusal(error: ApiError): string {
  const check = error.body?.check;
  return check ? `${check}: ${error.message}` : error.message;
}

/** The credentials a repository can name, read once for the screen. */
function useCredentials() {
  const [credentials, setCredentials] = useState<Credentials | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    listCredentials().then(setCredentials, (e: unknown) => setError(asError(e).message));
  }, []);
  return { credentials, error };
}

/**
 * The registered backup repositories: register one with its credential,
 * validating that Menagerie can write to it with that credential before it
 * saves, see which workspaces back up to each and what each authenticates
 * with, change that, and remove one no workspace backs up to.
 */
export function BackupRepositoriesScreen() {
  const [repositories, setRepositories] = useState<BackupRepository[] | null>(null);
  const [error, setError] = useState<ApiError | null>(null);
  const [registering, setRegistering] = useState(false);
  const { credentials, error: credentialsError } = useCredentials();

  const load = () =>
    listBackupRepositories()
      .then((list) => {
        setRepositories(list);
        setError(null);
      })
      .catch((e: unknown) => setError(asError(e)));
  useEffect(() => {
    void load();
  }, []);
  const open = () => setRegistering(true);

  return (
    <section className="screen templates backups" aria-labelledby="backups-title">
      <header className="screen-head">
        <h1 id="backups-title">Backup repositories</h1>
        <button type="button" className="primary" onClick={open} aria-expanded={registering}>
          Register repository
        </button>
      </header>
      <p className="muted backups-lede">
        Each workspace backs up to one of these repositories, on a branch named after the workspace.
        Choose the repository when you create the workspace.
      </p>
      {registering ? (
        <Registration
          credentials={credentials}
          credentialsError={credentialsError}
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
      {repositories === null && !error ? (
        <p className="muted">Reading backup repositories…</p>
      ) : null}
      {repositories?.length === 0 ? (
        <div className="empty">
          <p>No registered backup repositories. A workspace needs one before it can be created.</p>
          {registering ? null : (
            <button type="button" className="primary" onClick={open}>
              Register repository
            </button>
          )}
        </div>
      ) : null}
      {repositories?.length ? (
        <ul className="ledger template-ledger">
          {repositories.map((r) => (
            <RepositoryEntry
              key={r.id}
              repository={r}
              credentials={credentials}
              credentialsError={credentialsError}
              onChanged={(changed) =>
                setRepositories((list) =>
                  (list ?? []).map((x) => (x.id === changed.id ? changed : x)),
                )
              }
              onRemoved={() => setRepositories((list) => (list ?? []).filter((x) => x.id !== r.id))}
            />
          ))}
        </ul>
      ) : null}
    </section>
  );
}

function Registration({
  credentials,
  credentialsError,
  onRegistered,
}: {
  credentials: Credentials | null;
  credentialsError: string | null;
  onRegistered: () => void;
}) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [chosen, setChosen] = useState<BackupCredentialChoice | null>(null);
  const credential = effectiveChoice(url, credentials, chosen);
  const registration = (check: boolean) => ({
    name: name.trim(),
    url: url.trim(),
    ...(credential ? { credential } : {}),
    ...(check ? { check } : {}),
  });
  const { error, validated, busy, edited, run } = useValidated<BackupRepository>(
    JSON.stringify(registration(false)),
  );
  const filled = name.trim() !== "" && url.trim() !== "";

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!validated) return;
    if (await run("saving", () => registerBackupRepository(registration(false)))) onRegistered();
  };

  return (
    <form
      className="template-panel"
      aria-labelledby="register-backup-title"
      onSubmit={(e) => void submit(e)}
    >
      <h2 id="register-backup-title">Register backup repository</h2>
      <div className="field">
        <label className="field-label" htmlFor="backup-name">
          <span className="field-title">Display name</span>
        </label>
        <input
          id="backup-name"
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
        <label className="field-label" htmlFor="backup-url">
          <span className="field-title">Git URL</span>
        </label>
        <p className="field-description" id="backup-url-desc">
          An ssh, scp-like (git@host:path) or https URL. Validate checks that Menagerie can push to
          it, not branch protection: a rule that stops Menagerie creating or force-pushing a
          workspace&apos;s branch fails that workspace&apos;s backups.
        </p>
        <input
          id="backup-url"
          className="mono"
          required
          autoComplete="off"
          spellCheck={false}
          placeholder="git@github.com:example-org/workspace-backups.git"
          value={url}
          aria-describedby="backup-url-desc backup-registration-error"
          aria-invalid={error?.body?.check ? true : undefined}
          onChange={(e) => {
            setUrl(e.target.value);
            edited();
          }}
        />
      </div>
      <CredentialPicker
        id="backup-credential"
        url={url}
        credentials={credentials}
        loadError={credentialsError}
        value={credential}
        onChange={(choice) => {
          setChosen(choice);
          edited();
        }}
      />
      <p className="form-error" id="backup-registration-error" role="alert">
        {error ? refusal(error) : null}
      </p>
      <p className="form-success" role="status">
        {validated ? `Checks passed · Menagerie can write to it, as ${validated.id}` : null}
      </p>
      <div className="form-actions">
        <button
          type="button"
          disabled={busy !== null || !filled}
          onClick={() => void run("validating", () => registerBackupRepository(registration(true)))}
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

function RepositoryEntry({
  repository: r,
  credentials,
  credentialsError,
  onChanged,
  onRemoved,
}: {
  repository: BackupRepository;
  credentials: Credentials | null;
  credentialsError: string | null;
  onChanged: (changed: BackupRepository) => void;
  onRemoved: () => void;
}) {
  const [removeError, setRemoveError] = useState<ApiError | null>(null);
  const [removing, setRemoving] = useState(false);
  const [changing, setChanging] = useState(false);
  const remove = async () => {
    setRemoving(true);
    setRemoveError(null);
    try {
      await removeBackupRepository(r.id);
      onRemoved();
    } catch (e) {
      setRemoveError(asError(e));
      setRemoving(false);
    }
  };
  return (
    <li className="template-entry" aria-labelledby={`backup-${r.id}`}>
      <div>
        <h2 id={`backup-${r.id}`}>{r.name}</h2>
        <p className="muted mono template-address">{r.url}</p>
        <p className="muted">{describeCredential(r.credential, r.url, credentials)}</p>
        <p className="muted">
          {r.workspaces.length
            ? `Backs up ${r.workspaces.join(", ")}`
            : "No workspace backs up to it"}
        </p>
        {changing ? (
          <CredentialChange
            repository={r}
            credentials={credentials}
            credentialsError={credentialsError}
            onChanged={(changed) => {
              setChanging(false);
              onChanged(changed);
            }}
            onCancel={() => setChanging(false)}
          />
        ) : null}
        {removeError ? (
          <p className="form-error" role="alert">
            {removeError.message}
          </p>
        ) : null}
      </div>
      <div className="template-actions" role="group" aria-label={`Actions for ${r.name}`}>
        <button type="button" aria-expanded={changing} onClick={() => setChanging(!changing)}>
          Change credential
        </button>
        <button
          type="button"
          disabled={removing || r.workspaces.length > 0}
          title={
            r.workspaces.length
              ? "Move its workspaces' backups to another repository first"
              : "Forget this repository; its branches stay in it"
          }
          onClick={() => void remove()}
        >
          Remove
        </button>
      </div>
    </li>
  );
}

/** The current credential as a choice, so a change starts from it. */
function choiceOf(r: BackupRepository): BackupCredentialChoice | null {
  if (r.credential === null) return null;
  return "githubApp" in r.credential
    ? { githubApp: r.credential.githubApp }
    : { sshKey: r.credential.sshKey };
}

/** Change a repository's credential: Validate checks writing with it, Save records it. */
function CredentialChange({
  repository: r,
  credentials,
  credentialsError,
  onChanged,
  onCancel,
}: {
  repository: BackupRepository;
  credentials: Credentials | null;
  credentialsError: string | null;
  onChanged: (changed: BackupRepository) => void;
  onCancel: () => void;
}) {
  const [chosen, setChosen] = useState<BackupCredentialChoice | null>(choiceOf(r));
  const credential = effectiveChoice(r.url, credentials, chosen);
  const { error, validated, busy, edited, run } = useValidated<BackupRepository>(
    JSON.stringify(credential),
  );
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!validated || !credential) return;
    const changed = await run("saving", () => setBackupRepositoryCredential(r.id, credential));
    if (changed) onChanged(changed);
  };
  return (
    <form
      className="template-panel"
      aria-label={`Change the credential of ${r.name}`}
      onSubmit={(e) => void submit(e)}
    >
      <CredentialPicker
        id={`credential-${r.id}`}
        url={r.url}
        credentials={credentials}
        loadError={credentialsError}
        value={credential}
        onChange={(choice) => {
          setChosen(choice);
          edited();
        }}
      />
      <p className="form-error" role="alert">
        {error ? refusal(error) : null}
      </p>
      <p className="form-success" role="status">
        {validated ? "Checks passed · Menagerie can write to it with this credential" : null}
      </p>
      <div className="form-actions">
        <button
          type="button"
          disabled={busy !== null || !credential}
          onClick={() =>
            void run("validating", () => setBackupRepositoryCredential(r.id, credential!, true))
          }
        >
          {busy === "validating" ? "Checking…" : "Validate"}
        </button>
        <button type="submit" className="primary" disabled={busy !== null || !validated}>
          {busy === "saving" ? "Saving…" : "Save"}
        </button>
        <button type="button" className="ghost" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

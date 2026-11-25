import { useEffect, useState, type FormEvent } from "react";
import { ApiError, listBackupRepositories } from "../api/client.ts";
import type { BackupRepository, Workspace } from "../api/types.ts";
import { paths } from "../router.ts";
import { Link } from "./Link.tsx";

interface Props {
  workspace: Workspace;
  busy: boolean;
  onMove: (backupRepositoryId: string) => void;
  onCancel: () => void;
}

/**
 * Chooses another registered backup repository for a workspace's backups.
 * The move runs as a job, which proves the destination before it switches.
 */
export function MoveBackup({ workspace, busy, onMove, onCancel }: Props) {
  const [repositories, setRepositories] = useState<BackupRepository[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [chosen, setChosen] = useState("");

  useEffect(() => {
    listBackupRepositories()
      .then(setRepositories)
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : String(e)));
  }, []);

  const current = workspace.backupRepository?.id ?? null;
  const others = (repositories ?? []).filter((r) => r.id !== current);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (chosen) onMove(chosen);
  };
  const id = `move-backup-${workspace.name}`;

  return (
    <form
      className="entry-move"
      aria-label={`Move the backups of ${workspace.name}`}
      onSubmit={submit}
    >
      <p>
        Menagerie checks that it can write to the repository you choose, then commits and pushes a
        current backup there and reads it back before <span className="mono">{workspace.name}</span>{" "}
        backs up to it.
        {workspace.backupRepository
          ? ` Its earlier backups stay in ${workspace.backupRepository.name}.`
          : null}
      </p>
      {error ? (
        <p className="form-error" role="alert">
          {error}
        </p>
      ) : null}
      {repositories === null && !error ? (
        <p className="muted">Reading backup repositories…</p>
      ) : null}
      {repositories !== null && others.length === 0 ? (
        <p className="muted">
          No other backup repository is registered.{" "}
          <Link href={paths.backups()}>Register a backup repository</Link>.
        </p>
      ) : null}
      {others.length ? (
        <>
          <label className="field-label" htmlFor={id}>
            <span className="field-title">Move backups to</span>
          </label>
          <select id={id} value={chosen} autoFocus onChange={(e) => setChosen(e.target.value)}>
            <option value="">Choose a repository</option>
            {others.map((r) => (
              <option key={r.id} value={r.id}>
                {r.name} · {r.url}
              </option>
            ))}
          </select>
        </>
      ) : null}
      <span className="entry-delete-actions">
        <button type="submit" className="primary" disabled={busy || !chosen}>
          Move backups
        </button>
        <button type="button" className="ghost" onClick={onCancel}>
          Cancel
        </button>
      </span>
    </form>
  );
}

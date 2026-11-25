import { useEffect, useState } from "react";
import { ApiError, listBackupRepositories } from "../api/client.ts";
import type { BackupRepository } from "../api/types.ts";
import { paths } from "../router.ts";
import { Link } from "./Link.tsx";

interface Props {
  value: string | null;
  onChange: (id: string) => void;
}

/**
 * The registered backup repositories as a set of choice cards. The only
 * registered one is chosen for the person.
 */
export function BackupPicker({ value, onChange }: Props) {
  const [repositories, setRepositories] = useState<BackupRepository[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    listBackupRepositories()
      .then((list) => {
        setRepositories(list);
        if (list.length === 1 && list[0]) onChange(list[0].id);
      })
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : String(e)));
    // Read once: a later onChange must not choose again.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <fieldset className="field template-picker">
      <legend className="field-label">
        <span className="field-title">Backup repository</span>
        <span className="required" aria-label="required">
          ✱
        </span>
      </legend>
      <p className="field-description">
        Every backup of the workspace goes to a branch named after it in this repository.
      </p>
      {error ? (
        <p className="field-errors" role="alert">
          {error}
        </p>
      ) : null}
      {repositories === null && !error ? (
        <p className="muted">Reading backup repositories…</p>
      ) : null}
      {repositories?.length === 0 ? (
        <p className="muted">
          No registered backup repositories.{" "}
          <Link href={paths.backups()}>Register a backup repository</Link>.
        </p>
      ) : null}
      <div className="template-cards" role="radiogroup">
        {repositories?.map((r) => (
          <label key={r.id} className={`template-card${value === r.id ? " is-selected" : ""}`}>
            <input
              type="radio"
              name="backup-repository"
              value={r.id}
              checked={value === r.id}
              onChange={() => onChange(r.id)}
            />
            <span className="template-name">{r.name}</span>
            <span className="template-id mono backup-url" title={r.url}>
              {r.url}
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

import { useState, type FormEvent } from "react";
import type { ApiError } from "../api/client.ts";
import type { BackupCredential, BackupCredentialChoice, Credentials } from "../api/types.ts";
import { CredentialPicker, effectiveChoice, type CredentialPurpose } from "./CredentialPicker.tsx";
import { useValidated } from "./useValidated.ts";

/** The refusal's message, with the check it names. */
function refusal(error: ApiError): string {
  const check = error.body?.check;
  return check ? `${check}: ${error.message}` : error.message;
}

/** A recorded credential as a choice, so a change starts from it. */
export function choiceOf(credential: BackupCredential | null): BackupCredentialChoice | null {
  if (credential === null) return null;
  return "githubApp" in credential
    ? { githubApp: credential.githubApp }
    : { sshKey: credential.sshKey };
}

/**
 * Change a backup repository's or a template's credential: Validate runs
 * its checks with the new credential, Save records it. save runs one or
 * the other, by check.
 */
export function CredentialChange<T>({
  id,
  name,
  url,
  urlError = null,
  current,
  credentials,
  credentialsError,
  purpose,
  passed,
  save,
  onChanged,
  onCancel,
}: {
  id: string;
  name: string;
  url: string;
  urlError?: string | null;
  current: BackupCredential | null;
  credentials: Credentials | null;
  credentialsError: string | null;
  purpose: CredentialPurpose;
  /** What a passed Validate says. */
  passed: string;
  save: (credential: BackupCredentialChoice, check: boolean) => Promise<T>;
  onChanged: (changed: T) => void;
  onCancel: () => void;
}) {
  const [chosen, setChosen] = useState<BackupCredentialChoice | null>(choiceOf(current));
  const credential = effectiveChoice(url, credentials, chosen);
  const { error, validated, busy, edited, run } = useValidated<T>(JSON.stringify(credential));
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!validated || !credential) return;
    const changed = await run("saving", () => save(credential, false));
    if (changed) onChanged(changed);
  };
  return (
    <form
      className="template-panel"
      aria-label={`Change the credential of ${name}`}
      onSubmit={(e) => void submit(e)}
    >
      <CredentialPicker
        id={`credential-${id}`}
        url={url}
        urlError={urlError}
        purpose={purpose}
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
        {validated ? passed : null}
      </p>
      <div className="form-actions">
        <button
          type="button"
          disabled={busy !== null || !credential}
          onClick={() => void run("validating", () => save(credential!, true))}
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

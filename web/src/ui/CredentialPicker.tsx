import { useState } from "react";
import type {
  BackupCredential,
  BackupCredentialChoice,
  Credentials,
  SSHKeyChoice,
} from "../api/types.ts";
import { servingApps, urlKind } from "./credentialUrls.ts";

/** The credentials that fit url: keys over SSH, the Apps that serve it over https. */
function choicesFor(url: string, credentials: Credentials | null) {
  const kind = urlKind(url);
  return {
    kind,
    keys: kind === "ssh" ? (credentials?.sshKeys ?? []) : [],
    apps: kind === "https" ? servingApps(url, credentials?.githubApps ?? []) : [],
  };
}

/** Whether two App names name one App: the service compares them in any case. */
const sameApp = (a: string, b: string) => a.toLowerCase() === b.toLowerCase();

/**
 * The credential a form sends for url: chosen, while it still fits, an
 * App under its configured name; else, when none was chosen, the only
 * credential that fits; else none, which the service refuses where one
 * fits. A chosen credential that no longer fits is never swapped for
 * another unasked.
 */
export function effectiveChoice(
  url: string,
  credentials: Credentials | null,
  chosen: BackupCredentialChoice | null,
): BackupCredentialChoice | null {
  const { keys, apps } = choicesFor(url, credentials);
  if (chosen && "sshKey" in chosen)
    return keys.some((k) => k.publicKey === chosen.sshKey) ? chosen : null;
  if (chosen && "githubApp" in chosen) {
    const app = apps.find((a) => sameApp(a.name, chosen.githubApp));
    return app ? { githubApp: app.name } : null;
  }
  if (keys.length === 1) return { sshKey: keys[0]!.publicKey };
  if (apps.length === 1) return { githubApp: apps[0]!.name };
  return null;
}

/**
 * One line that names a repository's credential, for its entry. A
 * template's entry has no URL, only its kind, so an App's server goes
 * unchecked there.
 */
export function describeCredential(
  credential: BackupCredential | null,
  url: string | null,
  credentials: Credentials | null,
  kind: "ssh" | "https" | null = url === null ? null : urlKind(url),
): string {
  if (credential === null) {
    if (kind === "ssh") return "Credential chosen by URL: every key the SSH agent offers";
    if (kind === "https")
      return "Credential chosen by URL: the first GitHub App on its server, if any";
    return "Credential chosen by URL: the first GitHub App on its server, or every key the SSH agent offers";
  }
  if ("githubApp" in credential) {
    if (!credentials) return `GitHub App ${credential.githubApp}`;
    const app = credentials.githubApps.find((a) => sameApp(a.name, credential.githubApp));
    if (!app) return `GitHub App ${credential.githubApp}, which is no longer configured`;
    if (url !== null && servingApps(url, [app]).length === 0)
      return `GitHub App ${app.name}, which no longer serves this repository's server`;
    return `GitHub App ${app.name}`;
  }
  const key = credentials?.sshKeys.find((k) => k.publicKey === credential.sshKey);
  if (key) return `SSH key ${key.name} · ${credential.fingerprint}`;
  if (credentials && credentials.sshAgent === null)
    return `SSH key ${credential.fingerprint}, which the SSH agent no longer offers`;
  return `SSH key ${credential.fingerprint}`;
}

const keyLabel = (k: SSHKeyChoice) => `${k.name || "(unnamed)"} · ${k.fingerprint}`;

/** What a credential is for: a backup repository Menagerie writes to, or a template it reads. */
export type CredentialPurpose = "backup" | "template";

/**
 * The credential field of a backup repository or template form: the
 * agent's keys for an SSH URL, with the chosen key's public key to give
 * the Git host, and the Apps that serve an https URL. A template form
 * gives the URL its address names, and why that could not be read.
 */
export function CredentialPicker({
  id,
  url,
  credentials,
  loadError,
  value,
  onChange,
  purpose = "backup",
  urlError = null,
}: {
  id: string;
  url: string;
  credentials: Credentials | null;
  loadError: string | null;
  value: BackupCredentialChoice | null;
  onChange: (choice: BackupCredentialChoice | null) => void;
  purpose?: CredentialPurpose;
  urlError?: string | null;
}) {
  const { kind, keys, apps } = choicesFor(url, credentials);
  const describedBy = `${id}-desc`;
  let body;
  if (loadError) {
    body = <p className="form-error">The credentials could not be read: {loadError}</p>;
  } else if (credentials === null) {
    body = <p className="muted">Reading credentials…</p>;
  } else if (kind === null) {
    body =
      purpose === "template" ? (
        <p className={urlError ? "form-error" : "muted"}>
          {urlError ? `The address could not be read: ${urlError}` : "Give the address first."}
        </p>
      ) : (
        <p className="muted">Give the Git URL first.</p>
      );
  } else if (kind === "ssh") {
    body =
      keys.length === 0 ? (
        <p className="form-error">
          {credentials.sshAgent ??
            "The SSH agent offers no key. Offer one in Menagerie's Wyrwood policy."}
        </p>
      ) : (
        <>
          <select
            id={id}
            aria-describedby={describedBy}
            value={value && "sshKey" in value ? value.sshKey : ""}
            onChange={(e) => onChange(e.target.value ? { sshKey: e.target.value } : null)}
          >
            <option value="">Choose an SSH key…</option>
            {keys.map((k) => (
              <option key={k.publicKey} value={k.publicKey}>
                {keyLabel(k)}
              </option>
            ))}
          </select>
          {value && "sshKey" in value ? (
            <PublicKey publicKey={value.sshKey} purpose={purpose} />
          ) : null}
        </>
      );
  } else {
    body =
      apps.length === 0 ? (
        <p className="muted">
          No configured GitHub App serves this URL, so Menagerie sends it no credential.
        </p>
      ) : (
        <select
          id={id}
          aria-describedby={describedBy}
          value={value && "githubApp" in value ? value.githubApp : ""}
          onChange={(e) => onChange(e.target.value ? { githubApp: e.target.value } : null)}
        >
          <option value="">Choose a GitHub App…</option>
          {apps.map((a) => (
            <option key={a.name} value={a.name}>
              {a.name}
            </option>
          ))}
        </select>
      );
  }
  return (
    <div className="field">
      <label className="field-label" htmlFor={id}>
        <span className="field-title">Credential</span>
      </label>
      <p className="field-description" id={describedBy}>
        {purpose === "template"
          ? "Every Git operation on the template's repository uses this credential alone: an SSH key from Wyrwood's agent for an SSH address, or a GitHub App that serves an https address."
          : "Menagerie offers the repository this credential alone: an SSH key from Wyrwood's agent for an SSH URL, or a GitHub App that serves an https URL."}
      </p>
      {body}
    </div>
  );
}

/** A key's public key, ready to give the Git host. */
function PublicKey({ publicKey, purpose }: { publicKey: string; purpose: CredentialPurpose }) {
  const [copied, setCopied] = useState<string | null>(null);
  // The clipboard needs a secure context. Without one the key stays selectable.
  const copy = () => {
    void navigator.clipboard?.writeText(publicKey).then(
      () => setCopied(publicKey),
      () => undefined,
    );
  };
  return (
    <div className="credential-key" role="group" aria-label="Public key">
      <p>
        {purpose === "template"
          ? "Add this public key to the template's repository as a deploy key, or to an account that can read it, before you validate."
          : "Add this public key to the repository as a deploy key with write access, or to an account that can write to it, before you validate."}
      </p>
      <code className="mono">{publicKey}</code>
      <button type="button" onClick={copy}>
        {copied === publicKey ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

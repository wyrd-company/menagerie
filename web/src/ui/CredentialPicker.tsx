import { useState } from "react";
import type {
  BackupCredential,
  BackupCredentialChoice,
  Credentials,
  GitHubAppChoice,
  SSHKeyChoice,
} from "../api/types.ts";

// The URL rules below are the backend's: go-git's endpoint parsing for the
// kind of URL, and githubapp's Minter.Repository for the Apps serving it.
const schemePattern = /^[^:]+:\/\//;
const scpLikePattern = /^(?:[^@]+@)?[^:\s]+:(?:[0-9]{1,5}:)?[^\\].*$/;
const ownerPattern = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$/;
const namePattern = /^[A-Za-z0-9._-]+$/;

/** How a Git URL authenticates: over SSH with a key, over https with an App. */
export function urlKind(url: string): "ssh" | "https" | null {
  const u = url.trim();
  if (schemePattern.test(u)) {
    const scheme = u.slice(0, u.indexOf("://")).toLowerCase();
    return scheme === "ssh" ? "ssh" : scheme === "https" ? "https" : null;
  }
  // scp-like: [user@]host:path, with no scheme and no "/" before the first ":".
  if (scpLikePattern.test(u) && !u.slice(0, u.indexOf(":")).includes("/")) return "ssh";
  return null;
}

/** The host a URL names, lower case and without https's default port 443. */
function hostOf(url: string): string | null {
  try {
    return new URL(url).host;
  } catch {
    return null;
  }
}

/** Whether an App on server serves url: an https repository URL on its host. */
function serves(server: string, url: string): boolean {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return false;
  }
  if (u.protocol !== "https:" || u.username || u.password) return false;
  if (url.includes("?") || url.includes("#")) return false;
  if (u.host !== hostOf(server)) return false;
  const [owner = "", ...rest] = u.pathname.replace(/^\//, "").split("/");
  const name = rest.join("/").replace(/\.git$/, "");
  return (
    owner.length <= 100 &&
    ownerPattern.test(owner) &&
    name.length <= 100 &&
    namePattern.test(name) &&
    name !== "." &&
    name !== ".."
  );
}

/** The Apps that serve url, in configuration order. */
export function servingApps(url: string, apps: GitHubAppChoice[]): GitHubAppChoice[] {
  const u = url.trim();
  return apps.filter((a) => serves(a.server, u));
}

/** The credentials that fit url: keys over SSH, the Apps that serve it over https. */
function choicesFor(url: string, credentials: Credentials | null) {
  const kind = urlKind(url);
  return {
    kind,
    keys: kind === "ssh" ? (credentials?.sshKeys ?? []) : [],
    apps: kind === "https" ? servingApps(url, credentials?.githubApps ?? []) : [],
  };
}

/**
 * The credential a form sends for url: chosen, while it still fits; else
 * the only credential that fits; else none, which the service refuses
 * where one fits.
 */
export function effectiveChoice(
  url: string,
  credentials: Credentials | null,
  chosen: BackupCredentialChoice | null,
): BackupCredentialChoice | null {
  const { keys, apps } = choicesFor(url, credentials);
  if (chosen && "sshKey" in chosen && keys.some((k) => k.publicKey === chosen.sshKey))
    return chosen;
  if (chosen && "githubApp" in chosen && apps.some((a) => a.name === chosen.githubApp))
    return chosen;
  if (keys.length === 1) return { sshKey: keys[0]!.publicKey };
  if (apps.length === 1) return { githubApp: apps[0]!.name };
  return null;
}

/** One line that names a repository's credential, for its entry. */
export function describeCredential(
  credential: BackupCredential | null,
  url: string,
  credentials: Credentials | null,
): string {
  if (credential === null) {
    return urlKind(url) === "ssh"
      ? "Credential chosen by URL: every key the SSH agent offers"
      : "Credential chosen by URL: the first GitHub App on its server, if any";
  }
  if ("githubApp" in credential) return `GitHub App ${credential.githubApp}`;
  const key = credentials?.sshKeys.find((k) => k.publicKey === credential.sshKey);
  if (key) return `SSH key ${key.name} · ${credential.fingerprint}`;
  if (credentials && credentials.sshAgent === null)
    return `SSH key ${credential.fingerprint}, which the SSH agent no longer offers`;
  return `SSH key ${credential.fingerprint}`;
}

const keyLabel = (k: SSHKeyChoice) => `${k.name || "(unnamed)"} · ${k.fingerprint}`;

/**
 * The credential field of a backup repository form: the agent's keys for
 * an SSH URL, with the chosen key's public key to give the Git host, and
 * the Apps that serve an https URL.
 */
export function CredentialPicker({
  id,
  url,
  credentials,
  loadError,
  value,
  onChange,
}: {
  id: string;
  url: string;
  credentials: Credentials | null;
  loadError: string | null;
  value: BackupCredentialChoice | null;
  onChange: (choice: BackupCredentialChoice | null) => void;
}) {
  const { kind, keys, apps } = choicesFor(url, credentials);
  const describedBy = `${id}-desc`;
  let body;
  if (loadError) {
    body = <p className="form-error">The credentials could not be read: {loadError}</p>;
  } else if (credentials === null) {
    body = <p className="muted">Reading credentials…</p>;
  } else if (kind === null) {
    body = <p className="muted">Give the Git URL first.</p>;
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
          {value && "sshKey" in value ? <PublicKey publicKey={value.sshKey} /> : null}
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
        Menagerie offers the repository this credential alone: an SSH key from Wyrwood&apos;s agent
        for an SSH URL, or a GitHub App that serves an https URL.
      </p>
      {body}
    </div>
  );
}

/** A key's public key, ready to give the Git host. */
function PublicKey({ publicKey }: { publicKey: string }) {
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
        Add this public key to the repository as a deploy key with write access, or to an account
        that can write to it, before you validate.
      </p>
      <code className="mono">{publicKey}</code>
      <button type="button" onClick={copy}>
        {copied === publicKey ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

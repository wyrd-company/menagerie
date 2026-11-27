// Template versions as the workspace service orders them: MAJOR.MINOR.PATCH,
// with an optional leading v, by their numbers. A number is read whole, as
// a BigInt: a Number rounds one above 2^53 to its neighbour.
import { own } from "../own.ts";
import type { Template } from "./types.ts";

const numbers = (version: string) =>
  version
    .replace(/^v/, "")
    .split(".")
    .map((n) => (/^[0-9]+$/.test(n) ? BigInt(n) : 0n));

/** Whether version a is newer than version b. */
export function newer(a: string, b: string): boolean {
  const [x, y] = [numbers(a), numbers(b)];
  for (let i = 0; i < 3; i++) {
    const [p, q] = [x[i] ?? 0n, y[i] ?? 0n];
    if (p !== q) return p > q;
  }
  return false;
}

/** Whether a found version is newer than the template's current one. */
export const hasNewer = (t: Template) =>
  t.newestVersion !== null && newer(t.newestVersion, t.currentVersion);

/**
 * The versions a template can be upgraded to, newest first: the found
 * versions newer than its current one, and the current one itself while its
 * tag has moved.
 */
export function upgradeChoices(t: Template): string[] {
  const choices = t.versions
    .map((v) => v.version)
    .filter((v) => newer(v, t.currentVersion))
    .reverse();
  if (t.currentMoved) choices.push(t.currentVersion);
  return choices;
}

const failures: Record<string, string> = {
  "template-service-unavailable": "template service is unavailable",
  "template-not-found": "the Git host has no such repository",
  "template-resolve-failed": "the Git host refused or failed the listing",
  "invalid-address": "the template service refuses its address",
};

/** The failures the Git host answered, which name the credential it was offered. */
const hostRefused = new Set(["template-not-found", "template-resolve-failed"]);

/** A template's credential as a refusal names it; none for the rule chosen by URL. */
function phrase(credential: Template["credential"]): string {
  if (credential === null) return "";
  return "githubApp" in credential
    ? ` (fetched with GitHub App ${credential.githubApp})`
    : ` (fetched with SSH key ${credential.fingerprint})`;
}

/** How long ago a time was, as the templates screen says it. */
export function ago(at: string, now: number = Date.now()): string {
  const seconds = Math.max(0, Math.round((now - Date.parse(at)) / 1000));
  if (seconds < 60) return "just now";
  const unit = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"} ago`;
  if (seconds < 3600) return unit(Math.floor(seconds / 60), "minute");
  if (seconds < 86400) return unit(Math.floor(seconds / 3600), "hour");
  return unit(Math.floor(seconds / 86400), "day");
}

/** The last version check's line: when it ran, and why it failed. */
export function checkLine(t: Template, now: number = Date.now()): string {
  if (!t.lastCheck) return "Not checked yet";
  const when = ago(t.lastCheck.at, now);
  if (!t.lastCheck.failure) return `Checked ${when}`;
  const why = own(failures, t.lastCheck.failure) ?? t.lastCheck.failure;
  return `Check failed ${when}: ${why}${hostRefused.has(t.lastCheck.failure) ? phrase(t.credential) : ""}. Previously found versions are kept.`;
}

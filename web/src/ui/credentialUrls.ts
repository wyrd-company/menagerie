import type { GitHubAppChoice } from "../api/types.ts";

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

/**
 * A host with each "%XX" escape unescaped, as Go's url.Parse unescapes a
 * host it accepts, an IPv6 zone's included. The picker never refuses a
 * host itself: one Go refuses, it may read more loosely, and the service's
 * own refusal of the URL explains it.
 */
function decodeHost(raw: string): string {
  if (!raw.includes("%")) return raw;
  const bytes: number[] = [];
  const encoder = new TextEncoder();
  for (let i = 0; i < raw.length;) {
    const hex = raw.slice(i + 1, i + 3);
    if (raw[i] === "%" && /^[0-9A-Fa-f]{2}$/.test(hex)) {
      bytes.push(parseInt(hex, 16));
      i += 3;
      continue;
    }
    const char = String.fromCodePoint(raw.codePointAt(i)!);
    bytes.push(...encoder.encode(char));
    i += char.length;
  }
  // Go keeps bytes that are not UTF-8, and its lower case makes each U+FFFD.
  return new TextDecoder("utf-8").decode(new Uint8Array(bytes));
}

/**
 * An https URL's parts as Go's net/url reads them, from the raw text: no
 * dot segment is resolved and the host is unescaped and put in lower case
 * alone. Null for a URL that is not https or that holds a user name.
 */
function httpsParts(raw: string): { host: string; path: string; rest: string } | null {
  const m = /^([^:/?#]+):\/\/([^/?#]*)(.*)$/.exec(raw);
  if (!m || m[1]!.toLowerCase() !== "https" || m[2]!.includes("@")) return null;
  const host = decodeHost(m[2]!);
  const tail = m[3]!;
  const end = tail.search(/[?#]/);
  return {
    host: host.toLowerCase().replace(/:443$/, ""),
    path: end < 0 ? tail : tail.slice(0, end),
    rest: end < 0 ? "" : tail.slice(end),
  };
}

/**
 * Whether an App on server serves url, as githubapp's Minter.Repository
 * decides: an https URL on the server's host with no user name, no query,
 * not even an empty one, no fragment but an empty one, no escaped path,
 * and the path /{owner}/{name}, with name allowed a .git suffix.
 */
function serves(server: string, url: string): boolean {
  const u = httpsParts(url);
  const s = httpsParts(server);
  if (!u || !s || u.host !== s.host) return false;
  if (u.rest.startsWith("?") || u.rest.length > 1) return false;
  if (u.path.includes("%")) return false;
  const [owner = "", ...rest] = u.path.replace(/^\//, "").split("/");
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

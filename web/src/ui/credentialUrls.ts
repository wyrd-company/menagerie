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
    if (scheme !== "ssh" && scheme !== "https") return null;
    // Go's url.Parse refuses a host it cannot unescape, and go-git with it.
    return decodeHost(hostOf(u)) === null ? null : scheme;
  }
  // scp-like: [user@]host:path, with no scheme and no "/" before the first ":".
  if (scpLikePattern.test(u) && !u.slice(0, u.indexOf(":")).includes("/")) return "ssh";
  return null;
}

/** A URL's host and port: its authority, after "://" up to its path, from its last "@". */
function hostOf(url: string): string {
  const start = url.indexOf("://") + 3;
  const end = url.slice(start).search(/[/?#]/);
  const authority = end < 0 ? url.slice(start) : url.slice(start, start + end);
  return authority.slice(authority.lastIndexOf("@") + 1);
}

// The ASCII characters Go's url.Parse takes unescaped in a host.
const hostCharPattern = /^[A-Za-z0-9\-._~!$&'()*+,;=:[\]<>"]$/;

/**
 * A host as Go's url.Parse unescapes it, or null where Go refuses it: a
 * "%XX" escape stands for a byte that is not ASCII, or for "%" as "%25",
 * and any other ASCII character must be one a host takes unescaped.
 */
function decodeHost(raw: string): string | null {
  const bytes: number[] = [];
  const encoder = new TextEncoder();
  for (let i = 0; i < raw.length;) {
    const c = raw[i]!;
    if (c === "%") {
      const hex = raw.slice(i + 1, i + 3);
      if (!/^[0-9A-Fa-f]{2}$/.test(hex)) return null;
      const byte = parseInt(hex, 16);
      if (byte < 0x80 && byte !== 0x25) return null;
      bytes.push(byte);
      i += 3;
      continue;
    }
    const char = String.fromCodePoint(raw.codePointAt(i)!);
    if (char.length === 1 && c < "\x80" && !hostCharPattern.test(c)) return null;
    bytes.push(...encoder.encode(char));
    i += char.length;
  }
  // Go keeps bytes that are not UTF-8, and its lower case makes each U+FFFD.
  return new TextDecoder("utf-8").decode(new Uint8Array(bytes));
}

/**
 * An https URL's parts as Go's net/url reads them, from the raw text: no
 * dot segment is resolved and the host is unescaped and put in lower case
 * alone. Null for a URL that is not https, that holds a user name, or whose
 * host Go refuses.
 */
function httpsParts(raw: string): { host: string; path: string; rest: string } | null {
  const m = /^([^:/?#]+):\/\/([^/?#]*)(.*)$/.exec(raw);
  if (!m || m[1]!.toLowerCase() !== "https" || m[2]!.includes("@")) return null;
  const host = decodeHost(m[2]!);
  if (host === null) return null;
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

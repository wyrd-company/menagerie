import { useSyncExternalStore } from "react";

// Three page routes: the workspace list, create, and a workspace's upgrade.
export type Route =
  | { screen: "list"; jobId: string | null }
  | { screen: "create"; jobId: string | null }
  | { screen: "upgrade"; workspace: string; jobId: string | null }
  | { screen: "missing"; path: string };

export function parseRoute(pathname: string, search: string): Route {
  const jobId = new URLSearchParams(search).get("job");
  if (pathname === "/" || pathname === "") return { screen: "list", jobId };
  if (pathname === "/create") return { screen: "create", jobId };
  const upgrade = /^\/workspaces\/([^/]+)\/upgrade\/?$/.exec(pathname);
  if (upgrade?.[1]) {
    try {
      return { screen: "upgrade", workspace: decodeURIComponent(upgrade[1]), jobId };
    } catch {
      return { screen: "missing", path: pathname };
    }
  }
  return { screen: "missing", path: pathname };
}

export const paths = {
  list: (jobId?: string) => (jobId ? `/?job=${encodeURIComponent(jobId)}` : "/"),
  create: (jobId?: string) => (jobId ? `/create?job=${encodeURIComponent(jobId)}` : "/create"),
  upgrade: (workspace: string, jobId?: string) =>
    `/workspaces/${encodeURIComponent(workspace)}/upgrade${jobId ? `?job=${encodeURIComponent(jobId)}` : ""}`,
};

const listeners = new Set<() => void>();
function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("popstate", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("popstate", listener);
  };
}
const snapshot = () => window.location.pathname + window.location.search;

export function navigate(to: string, options: { replace?: boolean } = {}) {
  if (options.replace) window.history.replaceState(null, "", to);
  else window.history.pushState(null, "", to);
  for (const listener of listeners) listener();
  if (!options.replace) window.scrollTo({ top: 0 });
}

export function useRoute(): Route {
  const location = useSyncExternalStore(subscribe, snapshot);
  const url = new URL(location, "http://local");
  return parseRoute(url.pathname, url.search);
}

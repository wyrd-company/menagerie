import { useEffect } from "react";
import { paths, useRoute } from "./router.ts";
import { CreateWorkspace } from "./screens/CreateWorkspace.tsx";
import { UpgradeWorkspace } from "./screens/UpgradeWorkspace.tsx";
import { WorkspaceList } from "./screens/WorkspaceList.tsx";
import mark from "./brand/mark.svg";
import { Link } from "./ui/Link.tsx";

const titles = {
  list: "Workspaces",
  create: "New workspace",
  upgrade: "Upgrade",
  missing: "Not found",
};

export function App() {
  const route = useRoute();

  useEffect(() => {
    const name = route.screen === "upgrade" ? `Upgrade ${route.workspace}` : titles[route.screen];
    document.title = `${name} · Menagerie`;
  }, [route]);

  return (
    <div className="shell">
      <a className="skip" href="#main">
        Skip to content
      </a>
      <header className="masthead">
        <Link href={paths.list()} className="wordmark" aria-label="Menagerie, workspace list">
          <img className="wordmark-mark" src={mark} alt="" width="28" height="28" />
          <span className="wordmark-name">Menagerie</span>
        </Link>
        <nav aria-label="Main">
          <Link href={paths.list()} aria-current={route.screen === "list" ? "page" : undefined}>
            Workspaces
          </Link>
          <Link href={paths.create()} aria-current={route.screen === "create" ? "page" : undefined}>
            New workspace
          </Link>
        </nav>
        <form method="post" action="/auth/logout" className="signout">
          <button type="submit" className="ghost">
            Sign out
          </button>
        </form>
      </header>
      <main id="main" tabIndex={-1}>
        {route.screen === "list" ? <WorkspaceList jobId={route.jobId} /> : null}
        {route.screen === "create" ? <CreateWorkspace jobId={route.jobId} /> : null}
        {route.screen === "upgrade" ? (
          <UpgradeWorkspace key={route.workspace} workspace={route.workspace} jobId={route.jobId} />
        ) : null}
        {route.screen === "missing" ? (
          <section className="screen">
            <h1>Not found</h1>
            <p>
              Nothing lives at <span className="mono">{route.path}</span>.{" "}
              <Link href={paths.list()}>Go to the workspace list</Link>.
            </p>
          </section>
        ) : null}
      </main>
    </div>
  );
}

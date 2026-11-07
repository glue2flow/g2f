import { useEffect, useState } from "react";
import { api, logout } from "./api";
import type { ClientConfig, NodeDef, User } from "./types";
import { WorkflowList } from "./pages/WorkflowList";
import { Editor } from "./pages/Editor";
import { Plugins } from "./pages/Plugins";
import { Connections } from "./pages/Connections";

/** Minimal hash router: #/ and #/w/<id>. */
function useRoute(): string {
  const [hash, setHash] = useState(window.location.hash || "#/");
  useEffect(() => {
    const on = () => setHash(window.location.hash || "#/");
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return hash.slice(1);
}

export function navigate(path: string) {
  window.location.hash = path;
}

export function App({ config }: { config: ClientConfig }) {
  const route = useRoute();
  const [user, setUser] = useState<User>();
  const [defs, setDefs] = useState<NodeDef[]>();
  const [error, setError] = useState<string>();

  const loadDefs = () => api.nodes().then(setDefs);

  useEffect(() => {
    Promise.all([api.me(), api.nodes()])
      .then(([u, d]) => {
        setUser(u);
        setDefs(d);
      })
      .catch((e) => setError(e.message));
  }, []);

  if (error) return <div className="fatal">{error}</div>;
  if (!user || !defs) return <div className="loading">Loading…</div>;

  const editorMatch = route.match(/^\/w\/([\w-]+)$/);

  return (
    <div className="app">
      <header className="topbar">
        <a className="brand" href="#/">
          <img src="/appicon-64.png" alt="" /> glue2flow
        </a>
        <span className="org-chip" title="Organization">{user.org}</span>
        <nav className="nav">
          <a href="#/" className={route === "/" || route.startsWith("/w/") ? "active" : ""}>
            Workflows
          </a>
          <a href="#/connections" className={route === "/connections" ? "active" : ""}>
            Connections
          </a>
          <a href="#/plugins" className={route === "/plugins" ? "active" : ""}>
            Plugins
          </a>
        </nav>
        <div className="spacer" />
        <span className="user" title={user.groups.join("\n")}>
          {user.name || user.username}
          {user.admin && <span className="badge">admin</span>}
        </span>
        {config.authMode === "oidc" && (
          <button className="btn ghost" onClick={logout}>
            Sign out
          </button>
        )}
      </header>
      <main className="content">
        {editorMatch ? (
          <Editor key={editorMatch[1]} id={editorMatch[1]} defs={defs} user={user} config={config} />
        ) : route === "/plugins" ? (
          <Plugins user={user} config={config} onChanged={loadDefs} />
        ) : route === "/connections" ? (
          <Connections user={user} />
        ) : (
          <WorkflowList user={user} defs={defs} />
        )}
      </main>
    </div>
  );
}

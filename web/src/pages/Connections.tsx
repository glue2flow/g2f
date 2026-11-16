import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import { connectAccount } from "../lib/connect";
import type { Connection, ModuleInfo, Scope, User } from "../types";
import { NodeIcon } from "../components/NodeIcon";
import { CredentialDialog, credentialSummary } from "../components/CredentialDialog";

const scopeLabel: Record<Scope, string> = { private: "Only me", group: "Groups", org: "Organization" };

/**
 * Lists connected OAuth2 accounts and API-key credentials: connect or add,
 * edit, rename, share and remove.
 */
export function Connections({ user }: { user: User }) {
  const [list, setList] = useState<Connection[]>();
  const [modules, setModules] = useState<ModuleInfo[]>([]);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  /** Open credential dialog: "new", or the credential being edited. */
  const [editing, setEditing] = useState<Connection | "new">();

  const load = useCallback(() => {
    api
      .connections()
      .then(setList)
      .catch((e) => setError(e.message));
  }, []);
  useEffect(() => {
    load();
    api
      .plugins()
      .then((m) => setModules(m.filter((x) => x.oauth2 || x.credentials)))
      .catch(() => {});
  }, [load]);

  const name = (module: string) => modules.find((m) => m.module === module)?.name ?? module;
  const oauthModules = modules.filter((m) => m.oauth2);
  const credentialModules = modules.filter((m) => m.credentials);

  async function connect(module: string) {
    setError(undefined);
    setNotice(undefined);
    try {
      await connectAccount(module);
      setNotice(`${name(module)} account connected.`);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  async function update(c: Connection, patch: Parameters<typeof api.updateConnection>[1]) {
    try {
      const next = await api.updateConnection(c.id, patch);
      setList((l) => l?.map((x) => (x.id === c.id ? { ...x, ...next } : x)));
    } catch (e) {
      setError((e as Error).message);
    }
  }

  async function remove(c: Connection) {
    const what = c.kind === "credential" ? "credential" : "account";
    if (!confirm(`Remove "${c.name}"? Workflows using this ${what} will fail until another one is selected.`)) return;
    try {
      await api.deleteConnection(c.id);
      load();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <div className="list-page">
      <div className="list-head">
        <h1>Connections</h1>
        <div className="spacer" />
        {credentialModules.length > 0 && (
          <button className="btn primary" onClick={() => setEditing("new")}>
            🔑 Add credential
          </button>
        )}
        {oauthModules.map((m) => (
          <button key={m.module} className="btn" onClick={() => connect(m.module)}>
            <NodeIcon module={m} size={18} /> Connect {m.name}
          </button>
        ))}
      </div>
      <p className="muted small">
        Accounts you connected through OAuth2, and API-key credentials (tokens, webhook URLs) for services without OAuth2. Both
        are stored encrypted and only handed to the plugin while a node runs; workflows only reference them. Share one so others
        in your organization can select it in their nodes.
      </p>
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert info">{notice}</div>}
      {!list ? (
        <div className="loading">Loading…</div>
      ) : list.length === 0 ? (
        <div className="empty">
          {modules.length ? "No connected accounts or credentials yet." : "No installed plugin uses accounts or credentials."}
        </div>
      ) : (
        <table className="table compact-rows">
          <thead>
            <tr>
              <th>Name</th>
              <th>Plugin</th>
              <th>Type</th>
              <th>Account</th>
              <th>Owner</th>
              <th>Shared with</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.map((c) => (
              <tr key={c.id}>
                <td>
                  {c.canManage ? (
                    <input
                      className="title-input"
                      defaultValue={c.name}
                      onBlur={(e) => e.target.value !== c.name && update(c, { name: e.target.value })}
                    />
                  ) : (
                    <strong>{c.name}</strong>
                  )}
                </td>
                <td>
                  <span className="row-icon">
                    <NodeIcon module={modules.find((m) => m.module === c.module)} size={18} /> {name(c.module)}
                  </span>
                </td>
                <td>
                  {c.kind === "credential" ? (
                    <span className="chip kind-key" title="API-key credential">
                      🔑 API key
                    </span>
                  ) : (
                    <span className="chip kind-oauth" title="Connected through OAuth2">
                      OAuth
                    </span>
                  )}
                </td>
                <td className="muted">
                  {c.kind === "credential"
                    ? credentialSummary(c, modules.find((m) => m.module === c.module)) || "–"
                    : c.account || "–"}
                  {c.instanceUrl && <div title={c.instanceUrl}>{new URL(c.instanceUrl).host}</div>}
                </td>
                <td>{c.ownerSub === user.sub ? "you" : c.ownerName}</td>
                <td>
                  {c.canManage ? (
                    <div className="row">
                      <select value={c.scope} onChange={(e) => update(c, { scope: e.target.value as Scope })}>
                        {(Object.keys(scopeLabel) as Scope[]).map((s) => (
                          <option key={s} value={s}>
                            {scopeLabel[s]}
                          </option>
                        ))}
                      </select>
                      {c.scope === "group" && (
                        <input
                          list="my-groups-conn"
                          placeholder="/engineering, /sales"
                          defaultValue={c.groups.join(", ")}
                          onBlur={(e) =>
                            update(c, {
                              groups: e.target.value
                                .split(",")
                                .map((g) => g.trim())
                                .filter(Boolean),
                            })
                          }
                        />
                      )}
                    </div>
                  ) : (
                    scopeLabel[c.scope]
                  )}
                </td>
                <td>
                  {c.canManage && (
                    <div className="row">
                      {c.kind === "credential" && (
                        <button className="btn small ghost" onClick={() => setEditing(c)}>
                          Edit
                        </button>
                      )}
                      <button className="btn small ghost danger" onClick={() => remove(c)}>
                        Remove
                      </button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {editing && (
        <CredentialDialog
          modules={credentialModules}
          existing={editing === "new" ? undefined : editing}
          onClose={() => setEditing(undefined)}
          onSaved={(c) => {
            setEditing(undefined);
            setError(undefined);
            setNotice(editing === "new" ? `Credential "${c.name}" created.` : `Credential "${c.name}" saved.`);
            load();
          }}
        />
      )}
      <datalist id="my-groups-conn">
        {user.groups.map((g) => (
          <option key={g} value={g} />
        ))}
      </datalist>
    </div>
  );
}

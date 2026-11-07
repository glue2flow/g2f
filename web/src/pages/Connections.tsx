import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import { connectAccount } from "../lib/connect";
import type { Connection, ModuleInfo, Scope, User } from "../types";

const scopeLabel: Record<Scope, string> = { private: "Only me", group: "Groups", org: "Organization" };

/** Lists connected OAuth2 accounts: connect, rename, share and remove. */
export function Connections({ user }: { user: User }) {
  const [list, setList] = useState<Connection[]>();
  const [modules, setModules] = useState<ModuleInfo[]>([]);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();

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
      .then((m) => setModules(m.filter((x) => x.oauth2)))
      .catch(() => {});
  }, [load]);

  const name = (module: string) => modules.find((m) => m.module === module)?.name ?? module;

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
    if (!confirm(`Remove "${c.name}"? Workflows using this account will fail until another account is selected.`)) return;
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
        {modules.map((m) => (
          <button key={m.module} className="btn" onClick={() => connect(m.module)}>
            <span className="dot" style={{ background: m.color }} /> Connect {m.name}
          </button>
        ))}
      </div>
      <p className="muted small">
        Accounts you connected through OAuth2. Tokens are stored encrypted and refreshed automatically; workflows only reference
        the account. Share an account so others in your organization can select it in their nodes.
      </p>
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert info">{notice}</div>}
      {!list ? (
        <div className="loading">Loading…</div>
      ) : list.length === 0 ? (
        <div className="empty">{modules.length ? "No connected accounts yet." : "No installed plugin uses OAuth2 accounts."}</div>
      ) : (
        <table className="table compact-rows">
          <thead>
            <tr>
              <th>Name</th>
              <th>Plugin</th>
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
                <td>{name(c.module)}</td>
                <td className="muted">{c.account || "–"}</td>
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
                    <button className="btn small ghost danger" onClick={() => remove(c)}>
                      Remove
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <datalist id="my-groups-conn">
        {user.groups.map((g) => (
          <option key={g} value={g} />
        ))}
      </datalist>
    </div>
  );
}

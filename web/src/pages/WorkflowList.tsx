import { useEffect, useMemo, useState } from "react";
import { api } from "../api";
import { navigate } from "../App";
import type { Scope, User, Workflow } from "../types";

const scopeLabel: Record<Scope, string> = { private: "Private", group: "Groups", org: "Organization" };

export function WorkflowList({ user }: { user: User }) {
  const [items, setItems] = useState<Workflow[]>();
  const [filter, setFilter] = useState<"all" | "mine" | Scope>("all");
  const [error, setError] = useState<string>();

  useEffect(() => {
    api.workflows().then(setItems).catch((e) => setError(e.message));
  }, []);

  const visible = useMemo(
    () =>
      (items ?? []).filter((w) =>
        filter === "all" ? true : filter === "mine" ? w.ownerSub === user.sub : w.scope === filter,
      ),
    [items, filter, user.sub],
  );

  async function create() {
    try {
      const wf = await api.createWorkflow({
        name: "Untitled workflow",
        description: "",
        scope: "private",
        orgRole: "viewer",
        shares: [],
        active: false,
        graph: {
          nodes: [
            { id: "trigger", type: "trigger.manual", name: "Manual Trigger", position: { x: 80, y: 160 }, params: {} },
          ],
          edges: [],
        },
      });
      navigate(`/w/${wf.id}`);
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <div className="list-page">
      <div className="list-head">
        <h1>Workflows</h1>
        <div className="seg">
          {(["all", "mine", "private", "group", "org"] as const).map((f) => (
            <button key={f} className={filter === f ? "active" : ""} onClick={() => setFilter(f)}>
              {f === "all" ? "All" : f === "mine" ? "Mine" : scopeLabel[f]}
            </button>
          ))}
        </div>
        <div className="spacer" />
        <button className="btn primary" onClick={create}>
          + New workflow
        </button>
      </div>
      {error && <div className="alert error">{error}</div>}
      {!items ? (
        <div className="loading">Loading…</div>
      ) : visible.length === 0 ? (
        <div className="empty">No workflows yet. Create one to get started.</div>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Triggers</th>
              <th>Visibility</th>
              <th>Owner</th>
              <th>Your access</th>
              <th>Status</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {visible.map((w) => (
              <tr key={w.id} onClick={() => navigate(`/w/${w.id}`)}>
                <td>
                  <div className="wf-name">{w.name}</div>
                  {w.description && <div className="muted small">{w.description}</div>}
                </td>
                <td>
                  {(w.triggers ?? []).map((t) => (
                    <span key={t} className={`chip trig-${t}`}>
                      {t}
                    </span>
                  ))}
                </td>
                <td>
                  <span className={`chip scope-${w.scope}`}>{scopeLabel[w.scope]}</span>
                  {w.scope !== "private" && w.shares.length > 0 && (
                    <span className="muted small"> {w.shares.map((s) => s.group).join(", ")}</span>
                  )}
                </td>
                <td>{w.ownerSub === user.sub ? "you" : w.ownerName}</td>
                <td>{w.access}</td>
                <td>{w.active ? <span className="chip on">active</span> : <span className="chip">inactive</span>}</td>
                <td className="muted small">{new Date(w.updatedAt).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

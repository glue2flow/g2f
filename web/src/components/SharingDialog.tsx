import { useState } from "react";
import type { Role, Scope, Share, User } from "../types";

interface Props {
  user: User;
  scope: Scope;
  orgRole: Exclude<Role, "owner">;
  shares: Share[];
  canEdit: boolean;
  onSave: (v: { scope: Scope; orgRole: Exclude<Role, "owner">; shares: Share[] }) => void;
  onClose: () => void;
}

const roles: Exclude<Role, "owner">[] = ["viewer", "runner", "editor"];

export function SharingDialog({ user, canEdit, onSave, onClose, ...initial }: Props) {
  const [scope, setScope] = useState<Scope>(initial.scope);
  const [orgRole, setOrgRole] = useState(initial.orgRole);
  const [shares, setShares] = useState<Share[]>(initial.shares);
  const [group, setGroup] = useState("");

  const addShare = () => {
    const g = group.trim();
    if (!g || shares.some((s) => s.group === g)) return;
    setShares([...shares, { group: g.startsWith("/") ? g : "/" + g, role: "viewer" }]);
    setGroup("");
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>Sharing</h2>
        <div className="radio-list">
          {(
            [
              ["private", "Private", "Only you (and organization admins)."],
              ["group", "Groups", "You plus members of the groups below (including subgroups)."],
              ["org", "Organization", `Everyone in "${user.org}". Groups below can get more rights.`],
            ] as const
          ).map(([value, title, desc]) => (
            <label key={value} className={`radio ${scope === value ? "checked" : ""}`}>
              <input type="radio" disabled={!canEdit} checked={scope === value} onChange={() => setScope(value)} />
              <div>
                <strong>{title}</strong>
                <div className="muted small">{desc}</div>
              </div>
            </label>
          ))}
        </div>

        {scope === "org" && (
          <label className="field">
            <span className="field-label">Default role for organization members</span>
            <select disabled={!canEdit} value={orgRole} onChange={(e) => setOrgRole(e.target.value as typeof orgRole)}>
              {roles.map((r) => (
                <option key={r}>{r}</option>
              ))}
            </select>
          </label>
        )}

        {scope !== "private" && (
          <>
            <h3>Group access</h3>
            <table className="table compact">
              <tbody>
                {shares.map((s, i) => (
                  <tr key={s.group}>
                    <td>
                      <code>{s.group}</code>
                    </td>
                    <td>
                      <select
                        disabled={!canEdit}
                        value={s.role}
                        onChange={(e) => setShares(shares.map((x, j) => (j === i ? { ...x, role: e.target.value as Share["role"] } : x)))}
                      >
                        {roles.map((r) => (
                          <option key={r}>{r}</option>
                        ))}
                      </select>
                    </td>
                    <td>
                      {canEdit && (
                        <button className="btn ghost small" onClick={() => setShares(shares.filter((_, j) => j !== i))}>
                          Remove
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {canEdit && (
              <div className="row">
                <input
                  list="my-groups"
                  placeholder="/engineering"
                  value={group}
                  onChange={(e) => setGroup(e.target.value)}
                  onKeyDown={(e) => e.key === "Enter" && addShare()}
                />
                <datalist id="my-groups">
                  {user.groups.map((g) => (
                    <option key={g} value={g} />
                  ))}
                </datalist>
                <button className="btn" onClick={addShare}>
                  Add group
                </button>
              </div>
            )}
          </>
        )}

        <p className="muted small">
          Roles: <b>viewer</b> sees the workflow and its runs, <b>runner</b> can also run it, <b>editor</b> can change and
          activate it. Only the owner can change sharing or delete.
        </p>
        <div className="modal-actions">
          <button className="btn ghost" onClick={onClose}>
            {canEdit ? "Cancel" : "Close"}
          </button>
          {canEdit && (
            <button className="btn primary" onClick={() => onSave({ scope, orgRole, shares })}>
              Apply
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

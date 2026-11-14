import { useState } from "react";
import { api } from "../api";
import { SECRET_REF, type Connection, type ModuleInfo, type ParamDef } from "../types";
import { SecretField } from "./ParamPanel";

const isSecret = (f: ParamDef) => f.type === "secret" || f.type === "secretUrl";

/**
 * Creates an API-key credential, or edits the fields of one. The form comes
 * from the module's `credentials.fields`; secret fields are write-only, so
 * stored ones are sent back as SECRET_REF to keep them.
 */
export function CredentialDialog({
  modules,
  existing,
  onSaved,
  onClose,
}: {
  /** Modules to choose from (with credentials); a single one is preselected. */
  modules: ModuleInfo[];
  existing?: Connection;
  onSaved: (c: Connection) => void;
  onClose: () => void;
}) {
  const [module, setModule] = useState(existing?.module ?? (modules.length === 1 ? modules[0].module : ""));
  const [name, setName] = useState(existing?.name ?? "");
  const [draft, setDraft] = useState<Record<string, unknown>>(() => initialDraft(modules, existing));
  const [error, setError] = useState<string>();
  const [saving, setSaving] = useState(false);
  const info = modules.find((m) => m.module === module);
  const fields = info?.credentials?.fields ?? [];

  async function save() {
    setSaving(true);
    setError(undefined);
    try {
      const values = Object.fromEntries(fields.map((f) => [f.name, draft[f.name] ?? ""]));
      const c = existing
        ? await api.updateConnection(existing.id, { name: name.trim() || undefined, fields: values })
        : await api.createCredential(module, name, values);
      onSaved(c);
    } catch (e) {
      setError((e as Error).message);
      setSaving(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>{existing ? `Edit ${existing.name}` : info ? `New ${info.name} credential` : "New credential"}</h2>
        <p className="muted small">
          Stored encrypted. Secret values can be replaced but never read back, and only the plugin receives them, never node
          fields. The credential is private until you share it.
        </p>
        {error && <div className="alert error">{error}</div>}
        <div className="form">
          {!existing && modules.length > 1 && (
            <label className="field">
              <span className="field-label">
                Plugin<span className="req">*</span>
              </span>
              <select
                value={module}
                onChange={(e) => {
                  setModule(e.target.value);
                  setDraft({});
                }}
              >
                <option value="">Select a plugin…</option>
                {modules.map((m) => (
                  <option key={m.module} value={m.module}>
                    {m.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          {info && (
            <label className="field">
              <span className="field-label">Name</span>
              <input
                value={name}
                placeholder={`e.g. ${info.name} – deploy bot`}
                onChange={(e) => setName(e.target.value)}
              />
            </label>
          )}
          {fields.map((f) => {
            const label = (
              <span className="field-label">
                {f.label}
                {f.required && <span className="req">*</span>}
              </span>
            );
            const hosts =
              (f.type === "url" || f.type === "secretUrl") && info?.credentials?.apiHosts?.length ? (
                <span className="field-desc">Allowed hosts: {info.credentials.apiHosts.join(", ")}</span>
              ) : null;
            if (isSecret(f)) {
              // SecretField holds buttons: keep it out of a <label> (see ParamPanel Field).
              return (
                <div key={f.name} className="field">
                  {label}
                  <SecretField
                    value={draft[f.name]}
                    readOnly={false}
                    placeholder={f.placeholder}
                    onChange={(v) => setDraft({ ...draft, [f.name]: v ?? "" })}
                  />
                  {f.description && <span className="field-desc">{f.description}</span>}
                  {hosts}
                </div>
              );
            }
            return (
              <label key={f.name} className="field">
                {label}
                <input
                  value={String(draft[f.name] ?? "")}
                  placeholder={f.default !== undefined ? `default: ${String(f.default)}` : f.placeholder}
                  onChange={(e) => setDraft({ ...draft, [f.name]: e.target.value })}
                />
                {f.description && <span className="field-desc">{f.description}</span>}
                {hosts}
              </label>
            );
          })}
        </div>
        <div className="modal-actions">
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!info || saving} onClick={save}>
            {existing ? "Save" : "Create"}
          </button>
        </div>
      </div>
    </div>
  );
}

function initialDraft(modules: ModuleInfo[], c?: Connection): Record<string, unknown> {
  if (!c) return {};
  const fields = modules.find((m) => m.module === c.module)?.credentials?.fields ?? [];
  return Object.fromEntries(
    fields.map((f) => [f.name, isSecret(f) ? (c.secrets?.[f.name] ? SECRET_REF : "") : (c.fields?.[f.name] ?? "")]),
  );
}

/** Short summary of a credential's fields for lists: non-secret values and which secrets are set. */
export function credentialSummary(c: Connection, module?: ModuleInfo): string {
  const fields = module?.credentials?.fields ?? [];
  const parts: string[] = [];
  for (const f of fields) {
    if (isSecret(f)) {
      if (c.secrets?.[f.name]) parts.push(`${f.label} 🔒`);
    } else if (c.fields?.[f.name]) {
      parts.push(f.type === "url" ? hostOf(c.fields[f.name]) : c.fields[f.name]);
    }
  }
  return parts.join(" · ");
}

function hostOf(u: string): string {
  try {
    return new URL(u).host;
  } catch {
    return u;
  }
}

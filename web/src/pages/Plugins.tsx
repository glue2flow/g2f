import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api";
import { SecretField } from "../components/ParamPanel";
import { SECRET_REF, type ConfigValue, type ModuleInfo, type User } from "../types";

export function Plugins({ user, onChanged }: { user: User; onChanged: () => void }) {
  const [modules, setModules] = useState<ModuleInfo[]>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [configFor, setConfigFor] = useState<ModuleInfo>();
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const load = useCallback(() => {
    api.plugins().then(setModules).catch((e) => setError(e.message));
  }, []);
  useEffect(load, [load]);

  async function upload(file: File) {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      const info = await api.importPlugin(file);
      setNotice(`Installed ${info.name} ${info.version ?? ""} (${info.nodes.length} nodes).`);
      load();
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
      if (fileRef.current) fileRef.current.value = "";
    }
  }

  async function remove(m: ModuleInfo) {
    if (!confirm(`Uninstall plugin "${m.name}"? Workflows using its nodes will fail until it is installed again.`)) return;
    try {
      await api.deletePlugin(m.module);
      load();
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    }
  }

  return (
    <div className="list-page">
      <div className="list-head">
        <h1>Plugins</h1>
        <div className="spacer" />
        {user.pluginAdmin && (
          <>
            <input
              ref={fileRef}
              type="file"
              accept=".zip,.yaml,.yml"
              hidden
              onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])}
            />
            <button className="btn primary" disabled={busy} onClick={() => fileRef.current?.click()}>
              {busy ? "Installing…" : "⬆ Import plugin"}
            </button>
          </>
        )}
      </div>
      <p className="muted small">
        Import a <code>.yaml</code> manifest (HTTP nodes) or a <code>.zip</code> with <code>plugin.yaml</code> and a WebAssembly
        module. See <code>examples/plugins/telegram</code>. Importing requires the <code>g8g-plugin-admin</code> role; global plugin
        settings are configured per organization by admins.
      </p>
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert info">{notice}</div>}
      {!modules ? (
        <div className="loading">Loading…</div>
      ) : (
        <div className="cards">
          {modules.map((m) => (
            <div key={m.module} className="card">
              <div className="card-head">
                <span className="dot" style={{ background: m.color }} />
                <strong>{m.name}</strong>
                {m.version && <span className="muted small">v{m.version}</span>}
                <div className="spacer" />
                <span className={`chip rt-${m.runtime}`}>{m.runtime}</span>
              </div>
              {m.description && <p className="desc">{m.description}</p>}
              <div className="muted small">
                <code>{m.module}</code> · {m.nodes.length} node{m.nodes.length === 1 ? "" : "s"} ·{" "}
                {m.source === "imported" ? `imported by ${m.installedBy || "?"}` : m.source}
              </div>
              {m.allowedHosts && m.allowedHosts.length > 0 && (
                <div className="muted small">Network: {m.allowedHosts.join(", ")}</div>
              )}
              {m.runtime === "wasm" && (!m.allowedHosts || m.allowedHosts.length === 0) && (
                <div className="muted small">Network: none</div>
              )}
              <div className="card-actions">
                {user.admin && m.config.length > 0 && (
                  <button className="btn small" onClick={() => setConfigFor(m)}>
                    ⚙ Configure ({m.config.length})
                  </button>
                )}
                {user.pluginAdmin && m.source === "imported" && (
                  <button className="btn small ghost danger" onClick={() => remove(m)}>
                    Uninstall
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
      {configFor && <ConfigDialog module={configFor} org={user.org} onClose={() => setConfigFor(undefined)} />}
    </div>
  );
}

/** Edits a plugin's global configuration for the admin's organization. */
function ConfigDialog({ module, org, onClose }: { module: ModuleInfo; org: string; onClose: () => void }) {
  const [values, setValues] = useState<ConfigValue[]>();
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [error, setError] = useState<string>();
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    api
      .pluginConfig(module.module)
      .then((vs) => {
        setValues(vs);
        setDraft(Object.fromEntries(vs.map((v) => [v.name, v.type === "secret" ? (v.set ? SECRET_REF : "") : (v.value ?? "")])));
      })
      .catch((e) => setError(e.message));
  }, [module.module]);

  async function save() {
    setSaving(true);
    try {
      await api.setPluginConfig(module.module, draft);
      onClose();
    } catch (e) {
      setError((e as Error).message);
      setSaving(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>{module.name} settings</h2>
        <p className="muted small">
          Applies to all workflows of organization <b>{org}</b>. Values are encrypted at rest; secrets can be replaced but never
          read back. Nodes access them as <code>config.&lt;name&gt;</code>.
        </p>
        {error && <div className="alert error">{error}</div>}
        {!values ? (
          <div className="loading">Loading…</div>
        ) : (
          <div className="form">
            {values.map((v) => (
              <label key={v.name} className="field">
                <span className="field-label">
                  {v.label}
                  {v.required && <span className="req">*</span>}
                </span>
                {v.type === "secret" ? (
                  <SecretField
                    value={draft[v.name]}
                    readOnly={false}
                    onChange={(x) => setDraft({ ...draft, [v.name]: x ?? "" })}
                  />
                ) : v.type === "boolean" ? (
                  <input
                    type="checkbox"
                    checked={draft[v.name] === true || draft[v.name] === "true"}
                    onChange={(e) => setDraft({ ...draft, [v.name]: e.target.checked })}
                  />
                ) : v.type === "select" ? (
                  <select value={String(draft[v.name] ?? "")} onChange={(e) => setDraft({ ...draft, [v.name]: e.target.value })}>
                    <option value="">(default)</option>
                    {v.options?.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                ) : (
                  <input
                    value={String(draft[v.name] ?? "")}
                    placeholder={v.default !== undefined ? `default: ${String(v.default)}` : v.placeholder}
                    onChange={(e) => setDraft({ ...draft, [v.name]: e.target.value })}
                  />
                )}
                {v.description && <span className="field-desc">{v.description}</span>}
              </label>
            ))}
          </div>
        )}
        <div className="modal-actions">
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!values || saving} onClick={save}>
            Save
          </button>
        </div>
      </div>
    </div>
  );
}

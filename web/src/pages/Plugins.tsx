import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api";
import { SecretField } from "../components/ParamPanel";
import { SECRET_REF, type ClientConfig, type ConfigValue, type ModuleInfo, type User } from "../types";

export function Plugins({ user, config, onChanged }: { user: User; config: ClientConfig; onChanged: () => void }) {
  const [modules, setModules] = useState<ModuleInfo[]>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [configFor, setConfigFor] = useState<ModuleInfo>();
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const load = useCallback(() => {
    api
      .plugins()
      .then(setModules)
      .catch((e) => setError(e.message));
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
        module. See <code>examples/plugins/telegram</code>. Importing requires the <code>g2f-plugin-admin</code> role; global
        plugin settings are configured per organization by admins.
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
                {m.oauth2 && <span className="chip rt-oauth">OAuth2</span>}
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
                {user.admin && m.config.length + m.vars.length > 0 && (
                  <button className="btn small" onClick={() => setConfigFor(m)}>
                    ⚙ Settings ({m.config.length + m.vars.length})
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
      {configFor && (
        <ConfigDialog
          module={configFor}
          org={user.org}
          redirectUrl={config.oauthRedirectUrl}
          onClose={() => setConfigFor(undefined)}
        />
      )}
    </div>
  );
}

/** Edits a plugin's global configuration for the admin's organization. */
function ConfigDialog({
  module,
  org,
  redirectUrl,
  onClose,
}: {
  module: ModuleInfo;
  org: string;
  redirectUrl: string;
  onClose: () => void;
}) {
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
          read back.
        </p>
        {module.oauth2 && (
          <div className="info-box">
            Users connect their own {module.name} accounts on the <a href="#/connections">Connections</a> page. Register this
            redirect URI in the provider's OAuth app:
            <div className="row" style={{ marginTop: 6 }}>
              <code className="url">{redirectUrl}</code>
              <button className="btn small" onClick={() => navigator.clipboard?.writeText(redirectUrl)}>
                Copy
              </button>
            </div>
          </div>
        )}
        {error && <div className="alert error">{error}</div>}
        {!values ? (
          <div className="loading">Loading…</div>
        ) : (
          <>
            {(["config", "var"] as const).map((kind) => {
              const list = values.filter((v) => v.kind === kind);
              if (list.length === 0) return null;
              return (
                <section key={kind} className="settings-section">
                  <h3>{kind === "config" ? "Plugin configuration" : "Variables"}</h3>
                  <p className="muted small">
                    {kind === "config" ? (
                      <>Used only by the plugin itself (e.g. in request URLs or headers). Not available in node fields.</>
                    ) : (
                      <>
                        Usable in node fields as <code>{"{{ vars.<name> }}"}</code>.
                      </>
                    )}
                  </p>
                  <div className="form">
                    {list.map((v) => (
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
                          <select
                            value={String(draft[v.name] ?? "")}
                            onChange={(e) => setDraft({ ...draft, [v.name]: e.target.value })}
                          >
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
                        {v.kind === "var" && (
                          <span className="field-desc">
                            Use as <code>{`{{ vars.${v.name} }}`}</code>
                          </span>
                        )}
                      </label>
                    ))}
                  </div>
                </section>
              );
            })}
          </>
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

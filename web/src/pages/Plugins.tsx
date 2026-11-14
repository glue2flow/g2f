import { useCallback, useEffect, useState } from "react";
import { api, ApiError } from "../api";
import { SecretField } from "../components/ParamPanel";
import {
  SECRET_REF,
  type CatalogPlugin,
  type CatalogResponse,
  type ClientConfig,
  type ConfigValue,
  type ModuleInfo,
  type PluginConflict,
  type PluginSummary,
  type User,
} from "../types";
import { NodeIcon } from "../components/NodeIcon";

type Tab = "installed" | "catalog";

export function Plugins({ user, config, onChanged }: { user: User; config: ClientConfig; onChanged: () => void }) {
  const [modules, setModules] = useState<ModuleInfo[]>();
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [configFor, setConfigFor] = useState<ModuleInfo>();
  const [importing, setImporting] = useState(false);
  const [tab, setTab] = useState<Tab>("installed");
  const showCatalog = user.pluginAdmin && !!config.pluginCatalog;

  const load = useCallback(() => {
    api
      .plugins()
      .then(setModules)
      .catch((e) => setError(e.message));
  }, []);
  useEffect(load, [load]);

  function installed(info: ModuleInfo) {
    setError(undefined);
    setNotice(
      `Installed ${info.name} ${info.version ?? ""} (${info.nodes.length} nodes, ${info.signer ? `signed by ${info.signer}` : "unsigned"}).`,
    );
    load();
    onChanged();
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
        {showCatalog && (
          <div className="tabs" role="tablist" aria-label="Plugin views">
            {(["installed", "catalog"] as const).map((t) => (
              <button
                key={t}
                role="tab"
                aria-selected={tab === t}
                className={`tab${tab === t ? " active" : ""}`}
                onClick={() => setTab(t)}
              >
                {t === "installed" ? "Installed" : "Catalog"}
              </button>
            ))}
          </div>
        )}
        <div className="spacer" />
        {user.pluginAdmin && (
          <button className="btn primary" onClick={() => setImporting(true)}>
            ⬆ Import plugin
          </button>
        )}
      </div>
      <p className="muted small">
        Import a <code>.yaml</code> manifest (HTTP nodes) or a <code>.zip</code> with <code>plugin.yaml</code> and a WebAssembly
        module, optionally with its <code>.sig</code> signature. See <code>examples/plugins/telegram</code>. Importing requires the{" "}
        <code>g2f-plugin-admin</code> role; global plugin settings are configured per organization by admins.
        {config.pluginRequireSigned && <> This installation only accepts plugins signed by a trusted key.</>}
      </p>
      {error && <div className="alert error">{error}</div>}
      {notice && <div className="alert info">{notice}</div>}
      {tab === "catalog" && showCatalog ? (
        <CatalogTab onInstalled={installed} />
      ) : !modules ? (
        <div className="loading">Loading…</div>
      ) : (
        <div className="cards">
          {modules.map((m) => (
            <div key={m.module} className={`card${m.blocked ? " blocked" : ""}`}>
              <div className="card-head">
                <NodeIcon module={m} size={22} />
                <strong>{m.name}</strong>
                {m.version && <span className="muted small">v{m.version}</span>}
                <div className="spacer" />
                {m.oauth2 && <span className="chip rt-oauth">OAuth2</span>}
                <span className={`chip rt-${m.runtime}`}>{m.runtime}</span>
              </div>
              {m.description && <p className="desc">{m.description}</p>}
              <div className="muted small">
                <code>{m.module}</code> · {m.nodes.length} node{m.nodes.length === 1 ? "" : "s"} · {sourceLabel(m)}
              </div>
              <div className="small">
                <TrustChip module={m} />
              </div>
              {m.blocked && (
                <div className="alert error small" role="status">
                  Not loaded: {m.blocked}
                </div>
              )}
              {m.allowedHosts && m.allowedHosts.length > 0 && (
                <div className="muted small">Network: {m.allowedHosts.join(", ")}</div>
              )}
              {m.runtime === "wasm" && !m.blocked && (!m.allowedHosts || m.allowedHosts.length === 0) && (
                <div className="muted small">Network: none</div>
              )}
              <div className="card-actions">
                {user.admin && !m.blocked && m.config.length + m.vars.length > 0 && (
                  <button className="btn small" onClick={() => setConfigFor(m)}>
                    ⚙ Settings ({m.config.length + m.vars.length})
                  </button>
                )}
                {user.pluginAdmin && m.source === "imported" && (
                  <button className="btn small ghost danger" onClick={() => remove(m)} aria-label={`Uninstall ${m.name}`}>
                    Uninstall
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
      {importing && (
        <ImportDialog
          requireSigned={!!config.pluginRequireSigned}
          onClose={() => setImporting(false)}
          onInstalled={(info) => {
            setImporting(false);
            installed(info);
          }}
        />
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

function sourceLabel(m: ModuleInfo): string {
  if (m.source === "imported") return `imported by ${m.installedBy || "?"}`;
  if (m.source.startsWith("file:")) return `bundled file (${m.source.slice(5)})`;
  return m.source;
}

/** Shows who vouches for a plugin: the operator (files), a signer, or nobody. */
function TrustChip({ module: m }: { module: ModuleInfo }) {
  if (m.blocked) return <span className="chip st-error">blocked</span>;
  if (m.source !== "imported") return <span className="chip">{m.source === "builtin" ? "built-in" : "bundled"}</span>;
  if (m.signer) return <span className="chip on">✓ signed by {m.signer}</span>;
  return <span className="chip st-running">unsigned</span>;
}

function signerText(s: { signer?: string }): string {
  return s.signer ? `signed by ${s.signer}` : "unsigned";
}

/** The question asked before an imported plugin is replaced. */
function confirmReplace(c: PluginConflict): boolean {
  const cur: PluginSummary = c.current;
  const inc: PluginSummary = c.incoming;
  return confirm(
    `Replace ${cur.name} ${cur.version ?? ""} (${cur.blocked ? "blocked" : signerText(cur)}) with ${inc.version ?? "the uploaded version"} (${signerText(inc)})?\n\n` +
      "The plugin is shared by all organizations: their settings and tokens will be used by the new version.",
  );
}

/** Runs an install; on a conflict asks to replace and runs it again with replace. */
async function withReplace(run: (replace: boolean) => Promise<ModuleInfo>): Promise<ModuleInfo | undefined> {
  try {
    return await run(false);
  } catch (e) {
    const conflict = e instanceof ApiError && e.status === 409 ? (e.data?.conflict as PluginConflict | undefined) : undefined;
    if (!conflict) throw e;
    if (!confirmReplace(conflict)) return undefined;
    return run(true);
  }
}

function ImportDialog({
  requireSigned,
  onClose,
  onInstalled,
}: {
  requireSigned: boolean;
  onClose: () => void;
  onInstalled: (m: ModuleInfo) => void;
}) {
  const [file, setFile] = useState<File>();
  const [signature, setSignature] = useState<File>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit() {
    if (!file) return;
    setBusy(true);
    setError(undefined);
    try {
      const info = await withReplace((replace) => api.importPlugin(file, signature, replace));
      if (info) onInstalled(info);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" role="dialog" aria-modal="true" aria-labelledby="import-title" onClick={(e) => e.stopPropagation()}>
        <h2 id="import-title">Import plugin</h2>
        <p className="muted small">
          {requireSigned
            ? "This installation only accepts plugins signed by a trusted key: add the bundle's .sig file."
            : "Unsigned plugins are accepted and shown as unsigned. A signature that does not verify is always refused."}
        </p>
        <div className="form">
          <div className="field">
            <label className="field-label" htmlFor="plugin-file">
              Plugin bundle (.zip or .yaml)<span className="req">*</span>
            </label>
            <input id="plugin-file" type="file" accept=".zip,.yaml,.yml" onChange={(e) => setFile(e.target.files?.[0])} />
          </div>
          <div className="field">
            <label className="field-label" htmlFor="plugin-sig">
              Signature (.sig, optional)
            </label>
            <input id="plugin-sig" type="file" accept=".sig,.json" onChange={(e) => setSignature(e.target.files?.[0])} />
            <span className="field-desc">
              Created with <code>g2f plugin sign</code>; see docs/plugins.md.
            </span>
          </div>
        </div>
        {error && (
          <div className="alert error" role="alert">
            {error}
          </div>
        )}
        <div className="modal-actions">
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!file || busy} onClick={submit}>
            {busy ? "Installing…" : "Import"}
          </button>
        </div>
      </div>
    </div>
  );
}

function CatalogTab({ onInstalled }: { onInstalled: (m: ModuleInfo) => void }) {
  const [data, setData] = useState<CatalogResponse>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState<string>();

  const load = useCallback((refresh = false) => {
    setError(undefined);
    (refresh ? api.refreshCatalog() : api.catalog()).then(setData).catch((e) => setError(e.message));
  }, []);
  useEffect(() => load(), [load]);

  async function install(catalog: string, p: CatalogPlugin) {
    const replacing = p.state === "update" || p.state === "older" || p.state === "blocked" || p.state === "installed";
    if (
      replacing &&
      !confirmReplace({
        current: { module: p.module, name: p.name, version: p.installedVersion, signer: p.installedSigner, blocked: p.state === "blocked" ? "blocked" : undefined },
        incoming: { module: p.module, name: p.name, version: p.version, signer: p.keyId },
      })
    )
      return;
    setBusy(p.module);
    setError(undefined);
    try {
      const info = replacing
        ? await api.installFromCatalog(catalog, p.module, true)
        : await withReplace((replace) => api.installFromCatalog(catalog, p.module, replace));
      if (info) onInstalled(info);
      load();
    } catch (e) {
      setError(`${p.name}: ${(e as Error).message}`);
    } finally {
      setBusy(undefined);
    }
  }

  return (
    <section aria-label="Plugin catalog">
      <div className="row" style={{ marginBottom: 8 }}>
        <span className="muted small">
          {data?.requireSigned
            ? "Only entries signed by a trusted key can be installed."
            : "Signatures are verified on install; unsigned entries are installed as unsigned."}
        </span>
        <div className="spacer" />
        <button className="btn small" onClick={() => load(true)}>
          ↻ Refresh
        </button>
      </div>
      {error && (
        <div className="alert error" role="alert">
          {error}
        </div>
      )}
      {!data ? (
        <div className="loading">Loading…</div>
      ) : (
        data.catalogs.map((c) => (
          <div key={c.url} className="catalog">
            <h2 className="catalog-title">
              {c.name || c.url} <span className="muted small">{c.url}</span>
            </h2>
            {c.error ? (
              <div className="alert error">Catalog unavailable: {c.error}</div>
            ) : c.plugins.length === 0 ? (
              <p className="muted">This catalog lists no plugins.</p>
            ) : (
              <table className="table compact catalog-table">
                <thead>
                  <tr>
                    <th>Plugin</th>
                    <th>Version</th>
                    <th>Signature</th>
                    <th aria-label="Actions" />
                  </tr>
                </thead>
                <tbody>
                  {c.plugins.map((p) => (
                    <tr key={p.module}>
                      <td>
                        <strong>{p.name}</strong> <code className="small">{p.module}</code>
                        {p.runtime && <span className={`chip rt-${p.runtime}`} style={{ marginLeft: 6 }}>{p.runtime}</span>}
                        {p.description && <div className="desc">{p.description}</div>}
                        {p.homepage && (
                          <a className="small" href={p.homepage} target="_blank" rel="noreferrer noopener">
                            Homepage
                          </a>
                        )}
                      </td>
                      <td>
                        {p.version}
                        {p.installedVersion && p.installedVersion !== p.version && (
                          <div className="muted small">installed: {p.installedVersion}</div>
                        )}
                      </td>
                      <td>
                        {p.verification === "trusted" ? (
                          <span className="chip on">✓ {p.keyId}</span>
                        ) : p.verification === "unknown-key" ? (
                          <span className="chip st-error">unknown key {p.keyId}</span>
                        ) : (
                          <span className="chip st-running">unsigned</span>
                        )}
                      </td>
                      <td>
                        <CatalogAction plugin={p} busy={busy === p.module} onInstall={() => install(c.url, p)} />
                        {!p.installable && p.reason && <div className="muted small catalog-reason">{p.reason}</div>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        ))
      )}
    </section>
  );
}

function CatalogAction({ plugin: p, busy, onInstall }: { plugin: CatalogPlugin; busy: boolean; onInstall: () => void }) {
  if (p.state === "installed") {
    return (
      <button className="btn small" disabled aria-label={`${p.name} ${p.version} is installed`}>
        Installed
      </button>
    );
  }
  if (p.state === "bundled") {
    return (
      <button className="btn small" disabled aria-label={`${p.name} is bundled with this installation`}>
        Bundled
      </button>
    );
  }
  const label =
    p.state === "update" ? `Update to ${p.version}` : p.state === "older" ? `Install ${p.version}` : p.state === "blocked" ? "Reinstall" : "Install";
  return (
    <button
      className={`btn small${p.state === "available" || p.state === "update" ? " primary" : ""}`}
      disabled={!p.installable || busy}
      onClick={onInstall}
      aria-label={`${label}: ${p.name} ${p.version}`}
    >
      {busy ? "Installing…" : label}
    </button>
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
                    {list.map((v) => {
                      // Secret fields hold a button; keep them out of the <label> (see ParamPanel Field).
                      const Wrap = v.type === "secret" ? "div" : "label";
                      return (
                      <Wrap key={v.name} className="field">
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
                      </Wrap>
                      );
                    })}
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

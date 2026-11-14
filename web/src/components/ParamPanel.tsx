import { useContext, useEffect, useRef, useState } from "react";
import { api } from "../api";
import { connectAccount } from "../lib/connect";
import { insertExpression, type ContentGroup } from "../lib/dynamicContent";
import { DynamicContent } from "./DynamicContent";
import { FormBuilder } from "./FormBuilder";
import { CredentialDialog } from "./CredentialDialog";
import { ModulesContext } from "./NodeIcon";
import { isSecretRef, parseList, type Connection, type NodeDef, type ParamDef, type Workflow } from "../types";
import type { FlowNodeData } from "./FlowNode";

interface Props {
  nodeId: string;
  data: FlowNodeData;
  readOnly: boolean;
  /** Extra info rendered under the header, e.g. a webhook URL. */
  extra?: React.ReactNode;
  /** Fields of earlier nodes for the ⚡ dynamic content picker. */
  content?: ContentGroup[];
  onChange: (patch: Partial<FlowNodeData>) => void;
  onDelete: () => void;
}

export function ParamPanel({ nodeId, data, readOnly, extra, content, onChange, onDelete }: Props) {
  const def = data.def;
  const setParam = (name: string, value: unknown) => onChange({ params: { ...data.params, [name]: value } });

  return (
    <aside className="side-panel">
      <div className="panel-head">
        <span className="dot" style={{ background: def?.color }} />
        <input
          className="title-input"
          value={data.name}
          disabled={readOnly}
          onChange={(e) => onChange({ name: e.target.value })}
        />
      </div>
      <div className="muted small">
        {def?.name} · <code>{data.type}</code> · <code>{nodeId}</code>
      </div>
      {def?.description && <p className="desc">{def.description}</p>}
      {extra}
      <div className="form">
        {def?.params
          .filter((p) => visible(p, def, data.params))
          .map((p) => (
            <Field
              key={p.name}
              def={p}
              module={def.module}
              value={data.params[p.name]}
              readOnly={readOnly}
              content={def.trigger ? undefined : content}
              codeItem={data.params.mode === "each" ? "item" : "items[0]"}
              onChange={(v) => setParam(p.name, v)}
            />
          ))}
        {!def && <div className="alert error">This node type is not installed on the server.</div>}
      </div>
      {!readOnly && (
        <div className="panel-actions">
          <label className="check">
            <input type="checkbox" checked={!!data.disabled} onChange={(e) => onChange({ disabled: e.target.checked })} />
            Disabled (pass items through)
          </label>
          <button className="btn danger" onClick={onDelete}>
            Delete node
          </button>
        </div>
      )}
      {def && !def.trigger && (
        <p className="hint">
          Use <code>{"{{ json.field }}"}</code> for the current item, <code>{"{{ nodes['Name'][0].field }}"}</code> for earlier
          nodes, <code>{"{{ trigger.field }}"}</code> for the trigger payload
          {def.module !== "builtin" && (
            <>
              , <code>{"{{ vars.name }}"}</code> for the plugin's organization variables
            </>
          )}
          .
        </p>
      )}
    </aside>
  );
}

function Field({
  def,
  module,
  value,
  readOnly,
  content,
  codeItem,
  onChange,
}: {
  def: ParamDef;
  module: string;
  value: unknown;
  readOnly: boolean;
  content?: ContentGroup[];
  /** What `json` is called in code fields (item or items[0]). */
  codeItem: string;
  onChange: (v: unknown) => void;
}) {
  const v = value ?? def.default;
  const ref = useRef<HTMLInputElement & HTMLTextAreaElement>(null);
  const isCode = def.type === "code";
  const canPick =
    !!content && !readOnly && (isCode ? true : !def.noExpression && ["string", "text", "json", "", undefined].includes(def.type));
  // Caret of the last edit; until the user placed one, insert at the end.
  const sel = useRef<[number, number]>(undefined);
  const pick = (expr: string) => {
    const el = ref.current;
    const cur = el?.value ?? String(v ?? "");
    const [start, end] = sel.current && sel.current[1] <= cur.length ? sel.current : [cur.length, cur.length];
    const r = insertExpression(cur, start, end, expr, isCode ? "code" : def.type === "json" ? "json" : "text");
    if (def.type === "json") jsonInsert.current?.(r.value);
    else onChange(r.value);
    sel.current = [r.caret, r.caret];
    requestAnimationFrame(() => {
      el?.focus();
      el?.setSelectionRange(r.caret, r.caret);
    });
  };
  const jsonInsert = useRef<(text: string) => void>(undefined);
  const label = (
    <span className="field-label">
      {def.label}
      {def.required && <span className="req">*</span>}
    </span>
  );
  let input: React.ReactNode;
  switch (def.type) {
    case "boolean":
      return (
        <label className="field check">
          <input
            type="checkbox"
            disabled={readOnly}
            checked={v === true || v === "true"}
            onChange={(e) => onChange(e.target.checked)}
          />
          {label}
        </label>
      );
    case "formFields":
      // Not wrapped in <label>: the builder holds many inputs.
      return (
        <div className="field">
          {label}
          <FormBuilder value={v} readOnly={readOnly} onChange={onChange} />
          {def.description && <span className="field-desc">{def.description}</span>}
        </div>
      );
    case "select":
      input = (
        <select disabled={readOnly} value={String(v ?? "")} onChange={(e) => onChange(e.target.value)}>
          {def.options?.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      );
      break;
    case "text":
      input = (
        <textarea
          ref={ref}
          rows={4}
          disabled={readOnly}
          placeholder={def.placeholder}
          value={String(v ?? "")}
          onChange={(e) => onChange(e.target.value)}
        />
      );
      break;
    case "json":
      input = (
        <JsonField
          value={v}
          readOnly={readOnly}
          placeholder={def.placeholder}
          inputRef={ref}
          insertRef={jsonInsert}
          onChange={onChange}
        />
      );
      break;
    case "code":
      input = (
        <textarea
          ref={ref}
          className="mono code"
          rows={14}
          spellCheck={false}
          disabled={readOnly}
          value={String(v ?? "")}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => {
            // Tab inserts two spaces instead of leaving the field.
            if (e.key === "Tab" && !e.shiftKey) {
              e.preventDefault();
              const t = e.currentTarget;
              const pos = t.selectionStart;
              const next = t.value.slice(0, pos) + "  " + t.value.slice(t.selectionEnd);
              onChange(next);
              requestAnimationFrame(() => t.setSelectionRange(pos + 2, pos + 2));
            }
          }}
        />
      );
      break;
    case "workflow":
      input = <WorkflowPicker value={v} readOnly={readOnly} onChange={onChange} />;
      break;
    case "connection":
      input = <ConnectionPicker module={module} value={v} readOnly={readOnly} onChange={onChange} />;
      break;
    case "workflows":
      input = <WorkflowPicker multiple value={v} readOnly={readOnly} onChange={onChange} />;
      break;
    case "number":
      input = (
        <input
          disabled={readOnly}
          placeholder={def.placeholder}
          value={String(v ?? "")}
          onChange={(e) => {
            const s = e.target.value;
            onChange(s !== "" && !isNaN(Number(s)) ? Number(s) : s);
          }}
        />
      );
      break;
    case "datetime":
      input = (
        <input type="datetime-local" disabled={readOnly} value={String(v ?? "")} onChange={(e) => onChange(e.target.value)} />
      );
      break;
    case "secret":
      input = <SecretField value={value} readOnly={readOnly} placeholder={def.placeholder} onChange={onChange} />;
      break;
    default:
      input = (
        <input
          ref={ref}
          disabled={readOnly}
          placeholder={def.placeholder}
          value={String(v ?? "")}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
  if (canPick) {
    const remember = (e: React.SyntheticEvent) => {
      const t = e.target as HTMLInputElement;
      if (t === ref.current && t.selectionStart !== null) sel.current = [t.selectionStart, t.selectionEnd ?? t.selectionStart];
    };
    input = (
      <div
        className={`dc-wrap ${def.type === "string" || !def.type ? "single" : ""}`}
        onSelect={remember}
        onKeyUp={remember}
        onMouseUp={remember}
      >
        {input}
        <DynamicContent
          groups={content}
          code={isCode}
          mapBase={isCode ? (b) => (b === "json" ? codeItem : b) : undefined}
          onPick={pick}
        />
      </div>
    );
  }
  // Interactive children (SecretField's Replace button, nested checkboxes, buttons)
  // must not sit inside a <label>: clicks on the label text would be forwarded to them.
  const Wrap = def.type === "secret" || def.type === "workflows" || def.type === "connection" ? "div" : "label";
  return (
    <Wrap className="field">
      {label}
      {input}
      {def.description && <span className="field-desc">{def.description}</span>}
    </Wrap>
  );
}

/** Edits JSON as text; stores parsed objects when valid, raw strings otherwise (to allow expressions). */
function JsonField({
  value,
  readOnly,
  placeholder,
  inputRef,
  insertRef,
  onChange,
}: {
  value: unknown;
  readOnly: boolean;
  placeholder?: string;
  inputRef?: React.Ref<HTMLTextAreaElement>;
  /** Set to a function that replaces the text (dynamic content) and commits it. */
  insertRef?: React.RefObject<((text: string) => void) | undefined>;
  onChange: (v: unknown) => void;
}) {
  const toText = (v: unknown) => (v === undefined || v === null ? "" : typeof v === "string" ? v : JSON.stringify(v, null, 2));
  const [text, setText] = useState(toText(value));
  const [invalid, setInvalid] = useState(false);
  useEffect(() => setText(toText(value)), [value]);

  const commit = (text: string) => {
    if (text.trim() === "") return onChange(undefined);
    try {
      onChange(JSON.parse(text));
      setInvalid(false);
    } catch {
      // Not JSON: keep as string so {{ expressions }} still work.
      setInvalid(!text.includes("{{"));
      onChange(text);
    }
  };
  if (insertRef)
    insertRef.current = (t: string) => {
      setText(t);
      commit(t);
    };

  return (
    <>
      <textarea
        ref={inputRef}
        className={`mono ${invalid ? "invalid" : ""}`}
        rows={5}
        disabled={readOnly}
        placeholder={placeholder}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onBlur={() => commit(text)}
      />
      {invalid && <span className="field-desc error-text">Not valid JSON</span>}
    </>
  );
}

export function defaultParams(def: NodeDef): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const p of def.params) if (p.default !== undefined) out[p.name] = p.default;
  return out;
}

/** Evaluates a parameter's showIf condition against the current values. */
function visible(p: ParamDef, def: NodeDef, params: Record<string, unknown>): boolean {
  if (!p.showIf) return true;
  return Object.entries(p.showIf).every(([name, values]) => {
    const v = params[name] ?? def.params.find((x) => x.name === name)?.default;
    return values.includes(String(v ?? ""));
  });
}

/**
 * Secrets are write-only: the server returns a placeholder for stored values,
 * which is sent back unchanged to keep them. Typing replaces, Clear deletes.
 */
export function SecretField({
  value,
  readOnly,
  placeholder,
  onChange,
}: {
  value: unknown;
  readOnly: boolean;
  placeholder?: string;
  onChange: (v: unknown) => void;
}) {
  if (isSecretRef(value)) {
    return (
      <div className="secret-stored">
        <span className="chip on">🔒 stored encrypted</span>
        {!readOnly && (
          <>
            <button type="button" className="btn small" onClick={() => onChange("")}>
              Replace
            </button>
            <button type="button" className="btn small danger" onClick={() => onChange(null)}>
              Clear
            </button>
          </>
        )}
      </div>
    );
  }
  return (
    <input
      type="password"
      autoComplete="new-password"
      disabled={readOnly}
      placeholder={placeholder ?? "Enter secret – stored encrypted, never shown again"}
      value={typeof value === "string" ? value : ""}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

let workflowList: Promise<Workflow[]> | undefined;

/** Picks one workflow (Execute Workflow) or several (Error Trigger) the user can see. */
function WorkflowPicker({
  value,
  multiple,
  readOnly,
  onChange,
}: {
  value: unknown;
  multiple?: boolean;
  readOnly: boolean;
  onChange: (v: unknown) => void;
}) {
  const [list, setList] = useState<Workflow[]>();
  useEffect(() => {
    workflowList ??= api.workflows();
    workflowList.then(setList).catch(() => setList([]));
  }, []);
  const current = location.hash.split("/w/")[1];
  const options = (list ?? []).filter((w) => w.id !== current);
  if (!list) return <span className="muted small">Loading workflows…</span>;
  if (!multiple) {
    return (
      <select disabled={readOnly} value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
        <option value="">Select a workflow…</option>
        {options.map((w) => (
          <option key={w.id} value={w.id}>
            {w.name}
            {w.triggers?.includes("call") ? "" : " (no 'Called by Workflow' trigger)"}
          </option>
        ))}
      </select>
    );
  }
  const selected = parseList(value);
  return (
    <div className="check-list">
      {options.length === 0 && <span className="muted small">No other workflows.</span>}
      {options.map((w) => (
        <label key={w.id} className="check">
          <input
            type="checkbox"
            disabled={readOnly}
            checked={selected.includes(w.id)}
            onChange={(e) => onChange((e.target.checked ? [...selected, w.id] : selected.filter((x) => x !== w.id)).join(","))}
          />
          {w.name}
        </label>
      ))}
    </div>
  );
}

/**
 * Selects a connection of the node's plugin: a connected OAuth2 account (＋
 * Connect opens the provider popup) or an API-key credential (＋ Add opens
 * the credential form).
 */
function ConnectionPicker({
  module,
  value,
  readOnly,
  onChange,
}: {
  module: string;
  value: unknown;
  readOnly: boolean;
  onChange: (v: unknown) => void;
}) {
  const info = useContext(ModulesContext).get(module);
  const credentials = info?.credentials;
  const [list, setList] = useState<Connection[]>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [adding, setAdding] = useState(false);
  const load = () =>
    api
      .connections(module)
      .then(setList)
      .catch((e) => setError(e.message));
  useEffect(() => {
    load();
  }, [module]); // eslint-disable-line react-hooks/exhaustive-deps

  const selected = String(value ?? "");
  const known = !selected || list?.some((c) => c.id === selected);
  async function connect() {
    setBusy(true);
    setError(undefined);
    try {
      const id = await connectAccount(module);
      await load();
      onChange(id);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const what = credentials ? "credential" : "account";
  const empty = credentials?.optional
    ? "None (no credential)"
    : list && list.length === 0
      ? `No ${what} yet`
      : `Select a${credentials ? "" : "n"} ${what}…`;
  return (
    <div className="connection-picker">
      <div className="row">
        <select disabled={readOnly || !list} value={selected} onChange={(e) => onChange(e.target.value)}>
          <option value="">{empty}</option>
          {list?.map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
              {c.account ? ` – ${c.account}` : ""}
              {c.canManage ? "" : ` (shared by ${c.ownerName})`}
            </option>
          ))}
          {!known && <option value={selected}>{credentials ? "Credential" : "Account"} of another user</option>}
        </select>
        {!readOnly &&
          (credentials ? (
            <button type="button" className="btn small" onClick={() => setAdding(true)}>
              ＋ Add
            </button>
          ) : (
            <button type="button" className="btn small" disabled={busy} onClick={connect}>
              {busy ? "Waiting…" : "＋ Connect"}
            </button>
          ))}
      </div>
      {error && <span className="field-desc error-text">{error}</span>}
      {adding && info && (
        <CredentialDialog
          modules={[info]}
          onClose={() => setAdding(false)}
          onSaved={async (c) => {
            setAdding(false);
            await load();
            onChange(c.id);
          }}
        />
      )}
    </div>
  );
}

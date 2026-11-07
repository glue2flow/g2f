import { useEffect, useState } from "react";
import { isSecretRef, type NodeDef, type ParamDef } from "../types";
import type { FlowNodeData } from "./FlowNode";

interface Props {
  nodeId: string;
  data: FlowNodeData;
  readOnly: boolean;
  /** Extra info rendered under the header, e.g. a webhook URL. */
  extra?: React.ReactNode;
  onChange: (patch: Partial<FlowNodeData>) => void;
  onDelete: () => void;
}

export function ParamPanel({ nodeId, data, readOnly, extra, onChange, onDelete }: Props) {
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
            <Field key={p.name} def={p} value={data.params[p.name]} readOnly={readOnly} onChange={(v) => setParam(p.name, v)} />
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
  value,
  readOnly,
  onChange,
}: {
  def: ParamDef;
  value: unknown;
  readOnly: boolean;
  onChange: (v: unknown) => void;
}) {
  const v = value ?? def.default;
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
          rows={4}
          disabled={readOnly}
          placeholder={def.placeholder}
          value={String(v ?? "")}
          onChange={(e) => onChange(e.target.value)}
        />
      );
      break;
    case "json":
      input = <JsonField value={v} readOnly={readOnly} placeholder={def.placeholder} onChange={onChange} />;
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
          disabled={readOnly}
          placeholder={def.placeholder}
          value={String(v ?? "")}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
  return (
    <label className="field">
      {label}
      {input}
      {def.description && <span className="field-desc">{def.description}</span>}
    </label>
  );
}

/** Edits JSON as text; stores parsed objects when valid, raw strings otherwise (to allow expressions). */
function JsonField({
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
  const toText = (v: unknown) => (v === undefined || v === null ? "" : typeof v === "string" ? v : JSON.stringify(v, null, 2));
  const [text, setText] = useState(toText(value));
  const [invalid, setInvalid] = useState(false);
  useEffect(() => setText(toText(value)), [value]);

  return (
    <>
      <textarea
        className={`mono ${invalid ? "invalid" : ""}`}
        rows={5}
        disabled={readOnly}
        placeholder={placeholder}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onBlur={() => {
          if (text.trim() === "") return onChange(undefined);
          try {
            onChange(JSON.parse(text));
            setInvalid(false);
          } catch {
            // Not JSON: keep as string so {{ expressions }} still work.
            setInvalid(!text.includes("{{"));
            onChange(text);
          }
        }}
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

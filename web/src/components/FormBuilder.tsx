import { formKey, type FormFieldDef } from "../types";

const types: { value: FormFieldDef["type"]; label: string }[] = [
  { value: "text", label: "Short text" },
  { value: "textarea", label: "Long text" },
  { value: "choice", label: "Single choice" },
  { value: "multichoice", label: "Multiple choice" },
];

const hasOptions = (t: FormFieldDef["type"]) => t === "choice" || t === "multichoice";

/** Edits the questions of a Form Trigger (parameter type "formFields"). */
export function FormBuilder({
  value,
  readOnly,
  onChange,
}: {
  value: unknown;
  readOnly: boolean;
  onChange: (v: FormFieldDef[]) => void;
}) {
  const qs: FormFieldDef[] = Array.isArray(value) ? (value as FormFieldDef[]) : [];

  const update = (i: number, patch: Partial<FormFieldDef>) => {
    const next = qs.slice();
    const q = { ...next[i], ...patch };
    // The key follows the label until it is edited by hand.
    if (patch.label !== undefined && (!next[i].key || next[i].key === formKey(next[i].label ?? ""))) {
      q.key = formKey(patch.label);
    }
    if (patch.type && hasOptions(patch.type) && !q.options?.length) q.options = ["Option 1", "Option 2"];
    if (patch.type && !hasOptions(patch.type)) delete q.options;
    next[i] = q;
    onChange(next);
  };
  const move = (i: number, d: number) => {
    const next = qs.slice();
    [next[i], next[i + d]] = [next[i + d], next[i]];
    onChange(next);
  };
  const remove = (i: number) => onChange(qs.filter((_, j) => j !== i));
  const add = () => {
    const keys = new Set(qs.map((q) => q.key));
    let n = qs.length + 1;
    while (keys.has(`question_${n}`)) n++;
    onChange([...qs, { key: `question_${n}`, label: `Question ${n}`, type: "text" }]);
  };

  const dupes = new Set(qs.map((q) => q.key).filter((k, i, all) => k && all.indexOf(k) !== i));

  return (
    <div className="form-builder">
      {qs.length === 0 && <div className="muted small">No questions yet.</div>}
      {qs.map((q, i) => (
        <div className="fb-question" key={i}>
          <div className="row">
            <span className="fb-num">{i + 1}</span>
            <input
              className="fb-label"
              aria-label="Question"
              placeholder="Question"
              disabled={readOnly}
              value={q.label ?? ""}
              onChange={(e) => update(i, { label: e.target.value })}
            />
            {!readOnly && (
              <span className="fb-tools">
                <button type="button" className="btn small ghost" title="Move up" disabled={i === 0} onClick={() => move(i, -1)}>
                  ↑
                </button>
                <button
                  type="button"
                  className="btn small ghost"
                  title="Move down"
                  disabled={i === qs.length - 1}
                  onClick={() => move(i, 1)}
                >
                  ↓
                </button>
                <button type="button" className="btn small ghost danger" title="Remove question" onClick={() => remove(i)}>
                  ✕
                </button>
              </span>
            )}
          </div>
          <div className="row fb-meta">
            <select
              aria-label="Answer type"
              disabled={readOnly}
              value={q.type ?? "text"}
              onChange={(e) => update(i, { type: e.target.value as FormFieldDef["type"] })}
            >
              {types.map((t) => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </select>
            <label className="check small">
              <input
                type="checkbox"
                disabled={readOnly}
                checked={!!q.required}
                onChange={(e) => update(i, { required: e.target.checked })}
              />
              Required
            </label>
          </div>
          {hasOptions(q.type) && (
            <label className="field">
              <span className="field-desc">Options, one per line</span>
              <textarea
                rows={Math.min(8, Math.max(3, (q.options?.length ?? 0) + 1))}
                disabled={readOnly}
                value={(q.options ?? []).join("\n")}
                onChange={(e) => update(i, { options: e.target.value.split("\n") })}
              />
            </label>
          )}
          <details className="fb-more">
            <summary className="small muted">
              Key <code>{q.key || formKey(q.label ?? "")}</code>
              {dupes.has(q.key) && <span className="error-text"> – duplicate</span>}
            </summary>
            <label className="field">
              <span className="field-desc">Key: the field name in the output item</span>
              <input
                className="mono"
                disabled={readOnly}
                value={q.key ?? ""}
                placeholder={formKey(q.label ?? "")}
                onChange={(e) => update(i, { key: e.target.value.replace(/[^A-Za-z0-9_]/g, "_") })}
              />
            </label>
            <label className="field">
              <span className="field-desc">Help text (optional)</span>
              <input disabled={readOnly} value={q.help ?? ""} onChange={(e) => update(i, { help: e.target.value })} />
            </label>
            {!hasOptions(q.type) && (
              <label className="field">
                <span className="field-desc">Placeholder (optional)</span>
                <input
                  disabled={readOnly}
                  value={q.placeholder ?? ""}
                  onChange={(e) => update(i, { placeholder: e.target.value })}
                />
              </label>
            )}
          </details>
        </div>
      ))}
      {!readOnly && (
        <button type="button" className="btn small" onClick={add}>
          + Add question
        </button>
      )}
    </div>
  );
}

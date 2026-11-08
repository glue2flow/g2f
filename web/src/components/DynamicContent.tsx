import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { formatPath, preview, type ContentGroup } from "../lib/dynamicContent";

interface Props {
  groups: ContentGroup[];
  /** Rewrites a group's base for the field, e.g. json -> item in code. */
  mapBase?: (base: string) => string;
  /** Code fields get bare expressions instead of {{ }}. */
  code?: boolean;
  onPick: (expr: string) => void;
}

/** ⚡ button at the end of a field; opens the list of fields of earlier nodes. */
export function DynamicContent({ groups, mapBase = (b) => b, code, onPick }: Props) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  const root = useRef<HTMLDivElement>(null);
  const pop = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<React.CSSProperties>();

  // The side panel scrolls and clips: the popover is fixed to the viewport,
  // below the button, or above it when there is more room there.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const r = root.current?.getBoundingClientRect();
      if (!r) return;
      const width = Math.min(340, window.innerWidth - 16);
      const left = Math.max(8, Math.min(r.right - width, window.innerWidth - width - 8));
      const below = window.innerHeight - r.bottom - 12;
      const above = r.top - 12;
      setPos(
        below >= 240 || below >= above
          ? { left, width, top: r.bottom + 4, maxHeight: Math.min(400, below) }
          : { left, width, bottom: window.innerHeight - r.top + 4, maxHeight: Math.min(400, above) },
      );
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (!root.current?.contains(t) && !pop.current?.contains(t)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const q = query.trim().toLowerCase();
  const pick = (expr: string) => {
    onPick(expr);
    setOpen(false);
    setQuery("");
  };

  return (
    // preventDefault keeps clicks from activating the surrounding <label>.
    <div className="dc" ref={root} onClick={(e) => e.preventDefault()}>
      <button
        type="button"
        className={`dc-button ${open ? "open" : ""}`}
        title="Insert dynamic content from earlier nodes"
        aria-label="Insert dynamic content"
        aria-expanded={open}
        // Keep focus (and the caret position) in the field.
        onMouseDown={(e) => e.preventDefault()}
        onClick={() => setOpen((o) => !o)}
      >
        ⚡
      </button>
      {open &&
        pos &&
        createPortal(
          <div className="dc-pop" role="dialog" aria-label="Dynamic content" ref={pop} style={pos}>
            <input
              className="dc-search"
              autoFocus
              placeholder="Search fields…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            <div className="dc-list">
              {groups.length === 0 && <div className="muted small pad">Connect an earlier node to use its output.</div>}
              {groups.map((g) => {
                const base = mapBase(g.base);
                const fields = g.fields.filter(
                  (f) => !q || f.path.join(".").toLowerCase().includes(q) || g.title.toLowerCase().includes(q),
                );
                if (q && fields.length === 0) return null;
                const isCollapsed = !q && collapsed[g.key];
                return (
                  <div key={g.key} className="dc-group">
                    <button
                      type="button"
                      className="dc-group-head"
                      onClick={() => setCollapsed((c) => ({ ...c, [g.key]: !c[g.key] }))}
                    >
                      <span className="dot" style={{ background: g.color ?? "var(--muted)" }} />
                      <strong>{g.title}</strong>
                      {base && <code className="dc-base">{base}</code>}
                      <span className="dc-caret">{isCollapsed ? "▸" : "▾"}</span>
                    </button>
                    {!isCollapsed && (
                      <>
                        {fields.length === 0 && g.note && <div className="dc-note">{g.note}</div>}
                        {fields.map((f) => {
                          const expr = formatPath(base, f.path);
                          return (
                            <button
                              type="button"
                              key={f.path.join(".")}
                              className="dc-field"
                              title={[expr, f.description].filter(Boolean).join("\n")}
                              onClick={() => pick(expr)}
                            >
                              <span className="dc-name">{formatPath("", f.path)}</span>
                              {f.type && <span className="dc-type">{f.type}</span>}
                              <span className="dc-sample">{preview(f.sample) || f.description}</span>
                            </button>
                          );
                        })}
                      </>
                    )}
                  </div>
                );
              })}
            </div>
            <div className="dc-foot muted small">
              {code ? (
                "Inserts the expression at the cursor."
              ) : (
                <>
                  Inserts <code>{"{{ … }}"}</code> at the cursor.
                </>
              )}{" "}
              <code>nodes['Name'][0]</code> is the first item of that node.
            </div>
          </div>,
          document.body,
        )}
    </div>
  );
}

import { useEffect, useMemo, useRef, useState } from "react";
import type { NodeDef } from "../types";

interface Props {
  defs: NodeDef[];
  onAdd: (def: NodeDef) => void;
}

const STORAGE_KEY = "g2f.palette.collapsed";

function loadCollapsed(): Set<string> {
  try {
    return new Set(JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "[]"));
  } catch {
    return new Set();
  }
}

function saveCollapsed(s: Set<string>) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify([...s]));
  } catch {
    // storage unavailable (private mode): state just isn't remembered
  }
}

interface Group {
  name: string;
  color?: string;
  items: NodeDef[];
}

/** Ranks a node for the search terms: 0 = no match, higher = better. */
function score(d: NodeDef, terms: string[]): number {
  if (terms.length === 0) return 1;
  const name = d.name.toLowerCase();
  const desc = (d.description ?? "").toLowerCase();
  const cat = d.category.toLowerCase();
  let total = 0;
  for (const t of terms) {
    if (name.startsWith(t)) total += 4;
    else if (name.includes(t)) total += 3;
    else if (cat.includes(t)) total += 2;
    else if (desc.includes(t)) total += 1;
    else return 0; // every term must match somewhere
  }
  return total;
}

/** Wraps the parts of text that match one of the terms in <mark>. */
function highlight(text: string, terms: string[]) {
  if (terms.length === 0 || !text) return text;
  const escaped = terms.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const parts = text.split(new RegExp(`(${escaped.join("|")})`, "ig"));
  return parts.map((p, i) => (i % 2 === 1 ? <mark key={i}>{p}</mark> : p));
}

/** The node catalogue: searchable, grouped into collapsible trees. */
export function NodePalette({ defs, onAdd }: Props) {
  const [query, setQuery] = useState("");
  const [collapsed, setCollapsed] = useState<Set<string>>(loadCollapsed);
  const [active, setActive] = useState(0);
  const searchRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  const terms = useMemo(() => query.toLowerCase().split(/\s+/).filter(Boolean), [query]);
  const searching = terms.length > 0;

  // Groups keep the server's category order; while searching, empty groups
  // disappear and items are sorted by relevance.
  const groups = useMemo<Group[]>(() => {
    const byCat = new Map<string, Group>();
    for (const d of defs) {
      const s = score(d, terms);
      if (!s) continue;
      let g = byCat.get(d.category);
      if (!g) byCat.set(d.category, (g = { name: d.category, color: d.color, items: [] }));
      g.items.push(d);
    }
    const list = [...byCat.values()];
    if (searching) for (const g of list) g.items.sort((a, b) => score(b, terms) - score(a, terms));
    return list;
  }, [defs, terms, searching]);

  const visibleItems = groups.flatMap((g) => (searching || !collapsed.has(g.name) ? g.items : []));
  const matchCount = searching ? groups.reduce((n, g) => n + g.items.length, 0) : 0;

  useEffect(() => setActive(0), [query]);

  // Keep the keyboard-selected item in view.
  useEffect(() => {
    listRef.current?.querySelector(".palette-item.active")?.scrollIntoView({ block: "nearest" });
  }, [active]);

  // "/" focuses the search unless the user is typing somewhere.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement;
      if (e.key === "/" && !["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName) && !el.isContentEditable) {
        e.preventDefault();
        searchRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  function toggle(name: string) {
    const next = new Set(collapsed);
    if (next.has(name)) next.delete(name);
    else next.add(name);
    setCollapsed(next);
    saveCollapsed(next);
  }

  function setAll(collapse: boolean) {
    const next = collapse ? new Set(groups.map((g) => g.name)) : new Set<string>();
    setCollapsed(next);
    saveCollapsed(next);
  }

  function onSearchKey(e: React.KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, visibleItems.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === "Enter" && visibleItems[active]) {
      e.preventDefault();
      onAdd(visibleItems[active]);
    } else if (e.key === "Escape") {
      setQuery("");
      searchRef.current?.blur();
    }
  }

  const allCollapsed = groups.length > 0 && groups.every((g) => collapsed.has(g.name));
  let index = -1;

  return (
    <aside className="palette">
      <div className="palette-search">
        <svg className="palette-search-icon" viewBox="0 0 16 16" aria-hidden>
          <circle cx="7" cy="7" r="4.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
          <path d="M10.5 10.5 14 14" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
        </svg>
        <input
          ref={searchRef}
          type="text"
          placeholder="Search nodes…"
          aria-label="Search nodes by name or description"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onSearchKey}
        />
        {query ? (
          <button
            className="palette-search-clear"
            aria-label="Clear search"
            onClick={() => {
              setQuery("");
              searchRef.current?.focus();
            }}
          >
            ×
          </button>
        ) : (
          <kbd className="palette-search-key" title="Press / to search">
            /
          </kbd>
        )}
      </div>
      <div className="palette-toolbar">
        <span className="muted small">
          {searching ? `${matchCount} match${matchCount === 1 ? "" : "es"}` : `${defs.length} nodes`}
        </span>
        {!searching && groups.length > 1 && (
          <button className="btn ghost small" onClick={() => setAll(!allCollapsed)}>
            {allCollapsed ? "Expand all" : "Collapse all"}
          </button>
        )}
      </div>

      <div className="palette-tree" ref={listRef} role="tree" aria-label="Nodes">
        {groups.length === 0 && <div className="muted small palette-empty">No nodes match “{query}”.</div>}
        {groups.map((g) => {
          const open = searching || !collapsed.has(g.name);
          return (
            <div key={g.name} className="palette-group" role="treeitem" aria-expanded={open}>
              <button className="palette-cat" onClick={() => !searching && toggle(g.name)} disabled={searching}>
                <svg className={`chevron ${open ? "open" : ""}`} viewBox="0 0 10 10" aria-hidden>
                  <path
                    d="M3.5 2 7 5 3.5 8"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
                <span className="palette-cat-name">{g.name}</span>
                <span className="palette-count">{g.items.length}</span>
              </button>
              {open && (
                <div className="palette-children" role="group">
                  {g.items.map((d) => {
                    index++;
                    const i = index;
                    return (
                      <div
                        key={d.type}
                        className={`palette-item ${searching && i === active ? "active" : ""}`}
                        role="treeitem"
                        draggable
                        title={d.description}
                        onDragStart={(e) => e.dataTransfer.setData("application/g2f-node", d.type)}
                        onDoubleClick={() => onAdd(d)}
                        onMouseEnter={() => searching && setActive(i)}
                      >
                        <span className="dot" style={{ background: d.color }} />
                        <span className="palette-item-text">
                          <span className="palette-item-name">{highlight(d.name, terms)}</span>
                          {searching && d.description && (
                            <span className="palette-item-desc">{highlight(d.description, terms)}</span>
                          )}
                        </span>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
      <p className="hint">{searching ? "Enter adds the highlighted node." : "Drag onto the canvas or double-click to add."}</p>
    </aside>
  );
}

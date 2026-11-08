// Dynamic content (⚡): lists the fields of earlier nodes so they can be inserted
// into a parameter as an expression, like Power Automate's dynamic content.
//
// Fields come from two sources, merged per node:
//  1. the recorded output of a run (selected or latest execution), with sample values;
//  2. the node type's declared outputSchema, so nodes that never ran are listed too.

import type { Edge } from "@xyflow/react";
import type { FlowNodeType } from "../components/FlowNode";
import type { Execution, OutputField, ParamDef } from "../types";

export type Segment = string | number;

export interface ContentField {
  path: Segment[];
  type?: string;
  sample?: unknown;
  description?: string;
}

export interface ContentGroup {
  key: string;
  title: string;
  /** Expression the field paths are appended to, e.g. json or nodes['HTTP'][0]. */
  base: string;
  fields: ContentField[];
  /** Shown when there are no fields. */
  note?: string;
  color?: string;
}

const MAX_DEPTH = 4;
const MAX_FIELDS = 150;

/** Upstream nodes of nodeId, closest first, with their distance. */
export function upstream(nodeId: string, edges: Edge[]): Map<string, number> {
  const dist = new Map<string, number>();
  let frontier = [nodeId];
  for (let d = 1; frontier.length > 0; d++) {
    const next: string[] = [];
    for (const id of frontier) {
      for (const e of edges) {
        if (e.target === id && e.source !== nodeId && !dist.has(e.source)) {
          dist.set(e.source, d);
          next.push(e.source);
        }
      }
    }
    frontier = next;
  }
  return dist;
}

function typeOf(v: unknown): string {
  if (v === null || v === undefined) return "any";
  if (Array.isArray(v)) return "array";
  return typeof v === "object" ? "object" : typeof v;
}

/** Flattens a sample value into field paths (objects recursively, arrays via their first element). */
function flatten(value: unknown, prefix: Segment[], out: Map<string, ContentField>, depth = 0) {
  if (out.size >= MAX_FIELDS || depth > MAX_DEPTH || value === null || typeof value !== "object") return;
  if (Array.isArray(value)) {
    if (value.length > 0 && typeof value[0] === "object" && value[0] !== null) flatten(value[0], [...prefix, 0], out, depth + 1);
    return;
  }
  for (const [k, v] of Object.entries(value)) {
    const path = [...prefix, k];
    const key = keyOf(path);
    if (!out.has(key)) out.set(key, { path, type: typeOf(v), sample: v });
    flatten(v, path, out, depth + 1);
  }
}

const keyOf = (path: Segment[]) => path.map(String).join("\u0000");

/** Adds b's fields to a; a wins (it has run data), but missing types/descriptions are filled in. */
function merge(a: Map<string, ContentField>, b: Iterable<ContentField>) {
  for (const f of b) {
    const k = keyOf(f.path);
    const cur = a.get(k);
    if (!cur) a.set(k, f);
    else
      a.set(k, {
        ...cur,
        type: cur.type && cur.type !== "any" ? cur.type : f.type,
        description: cur.description ?? f.description,
      });
  }
}

export interface Context {
  nodes: FlowNodeType[];
  edges: Edge[];
  execution?: Execution;
}

/** Fields of one node: run data merged with its declared schema. */
export function nodeFields(id: string, ctx: Context, seen = new Set<string>()): { fields: ContentField[]; dynamic: boolean } {
  if (seen.has(id)) return { fields: [], dynamic: false };
  seen.add(id);
  const out = new Map<string, ContentField>();
  const step = ctx.execution?.steps?.find((s) => s.nodeId === id);
  for (const item of Object.values(step?.output ?? {})
    .flat()
    .slice(0, 5))
    flatten(item, [], out);

  const node = ctx.nodes.find((n) => n.id === id);
  const schema = node?.data.def?.outputSchema;
  if (!node || !schema) return { fields: [...out.values()], dynamic: false };

  const param = (name: string) => node.data.params[name] ?? node.data.def?.params.find((p) => p.name === name)?.default;
  const fromParam = (name: string, prefix: Segment[]) => {
    let v = param(name);
    if (typeof v === "string") {
      try {
        v = JSON.parse(v);
      } catch {
        return;
      }
    }
    if (Array.isArray(v)) v = v[0];
    const m = new Map<string, ContentField>();
    flatten(v, prefix, m);
    // Values configured with {{ }} are not samples of the output.
    for (const f of m.values()) if (typeof f.sample === "string" && f.sample.includes("{{")) f.sample = undefined;
    merge(out, m.values());
  };

  if (schema.fromParam) fromParam(schema.fromParam, []);
  for (const f of schema.fields ?? []) {
    merge(out, [declared(f)]);
    if (f.fromParam) fromParam(f.fromParam, f.path.split("."));
  }
  // Recorded output already contains the passed-through fields; only guess
  // them from the inputs for nodes that have not run.
  const passes = schema.passthrough && (!schema.passthroughIf || [true, "true"].includes(param(schema.passthroughIf) as never));
  if (passes && !step?.output) {
    for (const e of ctx.edges) if (e.target === id) merge(out, nodeFields(e.source, ctx, seen).fields);
  }
  return { fields: [...out.values()], dynamic: !!schema.dynamic };
}

function declared(f: OutputField): ContentField {
  return { path: f.path.split("."), type: f.type, description: f.description };
}

/** Field groups offered in a parameter of node `nodeId`. */
export function contentGroups(nodeId: string, ctx: Context, vars: ParamDef[] = []): ContentGroup[] {
  const self = ctx.nodes.find((n) => n.id === nodeId);
  if (!self) return [];
  const dist = upstream(nodeId, ctx.edges);
  const ups = [...dist.entries()]
    .sort((a, b) => a[1] - b[1])
    .map(([id]) => ctx.nodes.find((n) => n.id === id))
    .filter((n): n is FlowNodeType => !!n);
  const groups: ContentGroup[] = [];

  // json: the current item, from the directly connected node(s).
  const parents = ups.filter((n) => dist.get(n.id) === 1);
  if (parents.length > 0) {
    const m = new Map<string, ContentField>();
    let dynamic = false;
    for (const p of parents) {
      const r = nodeFields(p.id, ctx);
      merge(m, r.fields);
      dynamic ||= r.dynamic;
    }
    groups.push({
      key: "json",
      title: "Current item",
      base: "json",
      fields: [...m.values()],
      note: emptyNote(dynamic),
    });
  }

  for (const n of ups) {
    const r = nodeFields(n.id, ctx);
    groups.push({
      key: `node:${n.id}`,
      title: n.data.name,
      base: `nodes[${quote(n.data.name)}][0]`,
      fields: r.fields,
      note: emptyNote(r.dynamic),
      color: n.data.def?.color,
    });
  }

  const triggers = ups.filter((n) => n.data.def?.trigger);
  if (triggers.length > 0) {
    const m = new Map<string, ContentField>();
    for (const t of triggers) merge(m, nodeFields(t.id, ctx).fields);
    groups.push({ key: "trigger", title: "Trigger", base: "trigger", fields: [...m.values()], note: emptyNote(false) });
  }

  if (vars.length > 0) {
    groups.push({
      key: "vars",
      title: "Organization variables",
      base: "vars",
      fields: vars.map((v) => ({ path: [v.name], type: "string", description: v.label !== v.name ? v.label : v.description })),
    });
  }

  groups.push({
    key: "workflow",
    title: "Workflow",
    base: "",
    fields: [
      { path: ["workflow", "name"], type: "string" },
      { path: ["workflow", "id"], type: "string" },
      { path: ["execution", "id"], type: "string" },
    ],
  });
  return groups;
}

const emptyNote = (dynamic: boolean) =>
  dynamic
    ? "Output depends on the configuration. Run the workflow once to see its fields."
    : "Run the workflow once to see its fields.";

/** Single-quoted expression string literal. */
export function quote(s: string): string {
  return `'${s.replace(/\\/g, "\\\\").replace(/'/g, "\\'")}'`;
}

// Words that cannot follow a dot in the expr language.
const reserved = new Set([
  "in",
  "not",
  "and",
  "or",
  "matches",
  "contains",
  "startsWith",
  "endsWith",
  "let",
  "nil",
  "true",
  "false",
]);

/** Builds base.a.b[0]['odd key'] from a path. */
export function formatPath(base: string, path: Segment[]): string {
  let out = base;
  for (const s of path) {
    if (typeof s === "number") out += `[${s}]`;
    else if (/^[A-Za-z_][A-Za-z0-9_]*$/.test(s) && !reserved.has(s)) out += out === "" ? s : `.${s}`;
    else out += out === "" ? s : `[${quote(s)}]`;
  }
  return out;
}

/**
 * Inserts expr at the selection. Outside of {{ }} it is wrapped in braces
 * (mode "code": never); inside, it is inserted as is. In mode "json" a new
 * {{ }} outside of a JSON string is quoted, so the text stays valid JSON.
 */
export function insertExpression(
  text: string,
  start: number,
  end: number,
  expr: string,
  mode: "text" | "json" | "code" = "text",
) {
  const before = text.slice(0, start);
  const inside = before.lastIndexOf("{{") > before.lastIndexOf("}}");
  let ins = mode === "code" || inside ? expr : `{{ ${expr} }}`;
  if (mode === "json" && !inside && !inJSONString(before)) ins = `"${ins}"`;
  return { value: before + ins + text.slice(end), caret: start + ins.length };
}

/** Reports whether the end of a JSON text is inside a string literal. */
function inJSONString(s: string): boolean {
  let open = false;
  for (let i = 0; i < s.length; i++) {
    if (s[i] === "\\") i++;
    else if (s[i] === '"') open = !open;
  }
  return open;
}

/** Short text for a sample value. */
export function preview(v: unknown): string {
  if (v === undefined) return "";
  if (v === null) return "null";
  if (Array.isArray(v)) return `[${v.length}]`;
  if (typeof v === "object") return "{…}";
  const s = String(v);
  return s.length > 40 ? s.slice(0, 40) + "…" : s;
}

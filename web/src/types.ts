export type Scope = "private" | "group" | "org";
export type Role = "viewer" | "runner" | "editor" | "owner";

export interface User {
  sub: string;
  username: string;
  name: string;
  email: string;
  org: string;
  groups: string[];
  roles: string[];
  admin: boolean;
  pluginAdmin: boolean;
}

export interface Share {
  group: string;
  role: Exclude<Role, "owner">;
}

export interface GraphNode {
  id: string;
  type: string;
  name: string;
  position: { x: number; y: number };
  params: Record<string, unknown>;
  disabled?: boolean;
}

export interface GraphEdge {
  id: string;
  source: string;
  sourceHandle?: string | null;
  target: string;
  targetHandle?: string | null;
}

export interface Graph {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface Workflow {
  id: string;
  orgId: string;
  name: string;
  description: string;
  ownerSub: string;
  ownerName: string;
  scope: Scope;
  orgRole: Exclude<Role, "owner">;
  shares: Share[];
  active: boolean;
  graph: Graph;
  version: number;
  createdAt: string;
  updatedAt: string;
  access: Role;
  triggers?: string[];
}

export type ParamType =
  | "string"
  | "text"
  | "number"
  | "boolean"
  | "select"
  | "json"
  | "secret"
  | "datetime"
  | "code"
  | "workflow"
  | "workflows"
  | "connection";

export interface ParamDef {
  name: string;
  label: string;
  type: ParamType;
  description?: string;
  placeholder?: string;
  default?: unknown;
  required?: boolean;
  options?: { label: string; value: string }[];
  noExpression?: boolean;
  showIf?: Record<string, string[]>;
}

export interface NodeDef {
  type: string;
  name: string;
  category: string;
  description: string;
  color?: string;
  trigger?: "manual" | "schedule" | "webhook" | "call" | "error" | "poll";
  dynamicOutputs?: { param: string; extra?: string[] };
  inputs: string[];
  outputs: string[];
  params: ParamDef[];
  outputSchema?: OutputSchema;
  module: string;
  source: string;
}

/** Describes the items a node emits (see internal/nodes/schema.go). */
export interface OutputSchema {
  /** Items keep the fields of the input items. */
  passthrough?: boolean;
  /** A boolean parameter that must be true for passthrough (e.g. keepExisting). */
  passthroughIf?: string;
  /** A parameter whose value (object, or list of objects) describes the items. */
  fromParam?: string;
  fields?: OutputField[];
  /** Shape depends on code or another workflow: only run data can tell. */
  dynamic?: boolean;
}

export interface OutputField {
  path: string;
  type?: "string" | "number" | "boolean" | "object" | "array" | "any";
  description?: string;
  fromParam?: string;
}

export type ExecStatus = "queued" | "running" | "waiting" | "success" | "error";

export interface Step {
  nodeId: string;
  nodeName: string;
  nodeType: string;
  status: "success" | "error" | "skipped" | "waiting";
  startedAt: string;
  durationMs: number;
  inputCount: number;
  output?: Record<string, Record<string, unknown>[]>;
  truncated?: boolean;
  error?: string;
  logs?: LogEntry[];
}

export interface LogEntry {
  time: string;
  level: "debug" | "info" | "warn" | "error";
  message: string;
}

export interface Execution {
  id: string;
  workflowId: string;
  workflowVersion: number;
  triggerType: string;
  triggerNode: string;
  triggeredBy: string;
  status: ExecStatus;
  error?: string;
  steps?: Step[];
  resumeAt?: string;
  waitMode?: string;
  startedAt: string;
  finishedAt?: string;
}

export interface ClientConfig {
  authMode: "oidc" | "none";
  keycloakUrl?: string;
  realm?: string;
  clientId?: string;
  publicUrl: string;
  oauthRedirectUrl: string;
}

export const roleRank: Record<Role, number> = { viewer: 1, runner: 2, editor: 3, owner: 4 };
export const atLeast = (r: Role | undefined, min: Role) => !!r && roleRank[r] >= roleRank[min];

export interface TriggerInfo {
  nodeId: string;
  name: string;
  kind: "manual" | "schedule" | "webhook" | "call" | "error" | "poll";
  listening: boolean;
  error?: string;
  webhook?: { url: string; method: string; auth: "none" | "token" | "hmac"; header?: string; respond: string };
  schedule?: { mode: "cron" | "once"; timezone: string; next: string[] };
  poll?: { intervalMinutes: number; next: string };
}

export interface ModuleInfo {
  module: string;
  name: string;
  version?: string;
  description?: string;
  color?: string;
  runtime: "builtin" | "http" | "wasm" | "go";
  source: string;
  config: ParamDef[];
  vars: ParamDef[];
  allowedHosts?: string[];
  oauth2?: { authorizeUrl: string; tokenUrl: string; scopes: string[]; apiHosts?: string[] };
  nodes: string[];
  installedBy?: string;
  installedAt?: string;
}

export interface ConfigValue extends ParamDef {
  kind: "config" | "var";
  set: boolean;
  value?: unknown;
}

/** Placeholder the API returns instead of a stored secret. Sending it back keeps the value. */
export const SECRET_REF = { $secret: "stored" } as const;
export const isSecretRef = (v: unknown): boolean =>
  typeof v === "object" && v !== null && "$secret" in (v as Record<string, unknown>);

/** Splits a comma separated list (or array) into unique, trimmed entries. */
export function parseList(v: unknown): string[] {
  const raw = Array.isArray(v) ? v.map(String) : typeof v === "string" ? v.split(",") : [];
  const out: string[] = [];
  for (const r of raw) {
    const t = r.trim();
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
}

/** Output handles of a node, honouring parameter-driven outputs (Switch). */
export function outputsFor(def: NodeDef | undefined, params: Record<string, unknown>): string[] {
  if (!def) return ["main"];
  const dyn = def.dynamicOutputs;
  if (!dyn) return def.outputs;
  const p = def.params.find((x) => x.name === dyn.param);
  const out = parseList(params[dyn.param] ?? p?.default);
  for (const e of dyn.extra ?? []) if (!out.includes(e)) out.push(e);
  return out;
}

export interface Connection {
  id: string;
  module: string;
  name: string;
  account: string;
  ownerSub: string;
  ownerName: string;
  scope: Scope;
  groups: string[];
  scopes: string;
  expiresAt?: string;
  createdAt: string;
  canManage: boolean;
}

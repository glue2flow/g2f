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

export type ParamType = "string" | "text" | "number" | "boolean" | "select" | "json" | "secret";

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
}

export interface NodeDef {
  type: string;
  name: string;
  category: string;
  description: string;
  color?: string;
  trigger?: "manual" | "schedule" | "webhook";
  inputs: string[];
  outputs: string[];
  params: ParamDef[];
  source: string;
}

export type ExecStatus = "queued" | "running" | "success" | "error";

export interface Step {
  nodeId: string;
  nodeName: string;
  nodeType: string;
  status: "success" | "error" | "skipped";
  startedAt: string;
  durationMs: number;
  inputCount: number;
  output?: Record<string, Record<string, unknown>[]>;
  truncated?: boolean;
  error?: string;
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
  startedAt: string;
  finishedAt?: string;
}

export interface ClientConfig {
  authMode: "oidc" | "none";
  keycloakUrl?: string;
  realm?: string;
  clientId?: string;
  publicUrl: string;
}

export const roleRank: Record<Role, number> = { viewer: 1, runner: 2, editor: 3, owner: 4 };
export const atLeast = (r: Role | undefined, min: Role) => !!r && roleRank[r] >= roleRank[min];

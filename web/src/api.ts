import Keycloak from "keycloak-js";
import type { ClientConfig, ConfigValue, Connection, Execution, ModuleInfo, NodeDef, Scope, TriggerInfo, User, Workflow } from "./types";

let keycloak: Keycloak | null = null;

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

/** Loads server config and, when OIDC is enabled, logs in via Keycloak (PKCE). */
export async function initAuth(): Promise<ClientConfig> {
  const cfg: ClientConfig = await fetch("/api/config").then((r) => r.json());
  if (cfg.authMode === "oidc") {
    keycloak = new Keycloak({ url: cfg.keycloakUrl!, realm: cfg.realm!, clientId: cfg.clientId! });
    await keycloak.init({ onLoad: "login-required", pkceMethod: "S256", checkLoginIframe: false });
    // Refresh shortly before expiry.
    setInterval(() => keycloak?.updateToken(60).catch(() => keycloak?.login()), 30_000);
  }
  return cfg;
}

export function logout() {
  keycloak?.logout({ redirectUri: window.location.origin });
}

export function accountUrl(): string | undefined {
  return keycloak?.createAccountUrl();
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  if (keycloak) await keycloak.updateToken(30).catch(() => keycloak?.login());
  const headers: Record<string, string> = {};
  if (keycloak?.token) headers.Authorization = `Bearer ${keycloak.token}`;
  let payload: BodyInit | undefined;
  if (body instanceof FormData) {
    payload = body; // browser sets the multipart boundary
  } else if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }
  const res = await fetch(path, { method, headers, body: payload });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error ?? res.statusText);
  return data as T;
}

export type WorkflowInput = Pick<
  Workflow,
  "name" | "description" | "scope" | "orgRole" | "shares" | "graph" | "active"
> & { version?: number };

export const api = {
  me: () => request<User>("GET", "/api/me"),
  nodes: () => request<NodeDef[]>("GET", "/api/nodes"),
  workflows: () => request<Workflow[]>("GET", "/api/workflows"),
  workflow: (id: string) => request<Workflow>("GET", `/api/workflows/${id}`),
  createWorkflow: (w: WorkflowInput) => request<Workflow>("POST", "/api/workflows", w),
  updateWorkflow: (id: string, w: WorkflowInput) => request<Workflow>("PUT", `/api/workflows/${id}`, w),
  deleteWorkflow: (id: string) => request<void>("DELETE", `/api/workflows/${id}`),
  run: (id: string, nodeId?: string, items?: unknown[]) =>
    request<Execution>("POST", `/api/workflows/${id}/run`, { nodeId, items }),
  executions: (id: string) => request<Execution[]>("GET", `/api/workflows/${id}/executions`),
  triggers: (id: string) => request<TriggerInfo[]>("GET", `/api/workflows/${id}/triggers`),
  connections: (module?: string) =>
    request<Connection[]>("GET", "/api/connections" + (module ? `?module=${encodeURIComponent(module)}` : "")),
  authorizeConnection: (module: string, name?: string) =>
    request<{ url: string }>("POST", "/api/connections/authorize", { module, name }),
  updateConnection: (id: string, patch: { name?: string; scope?: Scope; groups?: string[] }) =>
    request<Connection>("PATCH", `/api/connections/${id}`, patch),
  deleteConnection: (id: string) => request<void>("DELETE", `/api/connections/${id}`),
  plugins: () => request<ModuleInfo[]>("GET", "/api/plugins"),
  importPlugin: (file: File) => {
    const fd = new FormData();
    fd.append("file", file);
    return request<ModuleInfo>("POST", "/api/plugins", fd);
  },
  deletePlugin: (module: string) => request<void>("DELETE", `/api/plugins/${module}`),
  pluginConfig: (module: string) => request<ConfigValue[]>("GET", `/api/plugins/${module}/config`),
  setPluginConfig: (module: string, values: Record<string, unknown>) =>
    request<ConfigValue[]>("PUT", `/api/plugins/${module}/config`, { values }),
  cancelExecution: (id: string) => request<void>("POST", `/api/executions/${id}/cancel`),
  execution: (id: string) => request<Execution>("GET", `/api/executions/${id}`),
};

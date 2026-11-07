import Keycloak from "keycloak-js";
import type { ClientConfig, Execution, NodeDef, TriggerInfo, User, Workflow } from "./types";

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
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
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
  execution: (id: string) => request<Execution>("GET", `/api/executions/${id}`),
};

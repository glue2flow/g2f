[Docs](README.md) › Architecture

# Architecture

glue2flow is a single Go binary that serves the REST API, the React editor, webhooks and the
scheduler. All state lives in PostgreSQL.

- **Engine:** Go, single static binary, ~25 MB container image
- **Editor:** React + [React Flow](https://reactflow.dev)
- **Auth:** Keycloak / any OIDC provider (PKCE in the browser, JWT validation in the API)
- **Plugins:** YAML manifests (HTTP APIs) or sandboxed WebAssembly modules ([Extism](https://extism.org) on [wazero](https://wazero.io))
- **Code node:** sandboxed JavaScript ([goja](https://github.com/dop251/goja))

## Repository layout

```
cmd/g2f/             entrypoint (server + `g2f healthcheck`)
internal/config/     environment configuration
internal/auth/       OIDC token validation, User (groups, org, admin role)
internal/workflow/   workflow model, graph validation, sharing / roles
internal/expression/ {{ expr }} templates in node parameters
internal/nodes/      node registry, built-in nodes, manifest format, output schemas, HTTP modules
internal/plugin/     plugin import/sync, WebAssembly runtime (Extism + wazero)
internal/secrets/    AES-GCM encryption, node secrets, plugin configuration
internal/engine/     executes graphs, records executions
internal/trigger/    scheduler (cron / one-time) and webhook endpoint
internal/store/      PostgreSQL persistence + embedded migrations
internal/api/        REST API, SPA hosting
internal/mailmsg/    MIME e-mail builder (email module, mimeMessage() expression)
modules/             plugins loaded at startup (*.yaml, *.zip, folders); modules/email is Go
examples/plugins/    sample WebAssembly plugins (Telegram in Go and JS, Mistral AI in Go)
web/                 React editor
deploy/              Keycloak realm + Postgres init for docker-compose
docs/                this documentation
```

## Execution model

A run starts at one trigger node. Every node reachable from it runs once, in topological
order. Data flows as a list of JSON **items**; a node's input is the concatenation of all
items arriving on its incoming arrows. Nodes with several outputs (e.g. `If` → `true`/`false`)
route items to a specific handle; a node that receives no items is **skipped**.

Each execution stores per-node status, duration and output (capped at
`G2F_MAX_STORED_ITEMS` items per output), which the editor shows below the canvas.

The engine runs acyclic graphs only. Loops are built with
[Execute Workflow](nodes.md#execute-workflow) (per item or per batch).

## Scaling and replicas

| Part | Several replicas |
| --- | --- |
| Schedules | Safe: every replica runs the scheduler, each firing is claimed once in PostgreSQL (`trigger_firings`). |
| Wait nodes | Safe: every replica polls for due waits, each resume is claimed atomically. |
| Imported plugins | Safe: stored in PostgreSQL and reloaded by every replica (`G2F_PLUGIN_SYNC`). |
| Running executions after a restart | Not yet: recovery assumes a single instance. A Postgres-backed job queue is on the [roadmap](../README.md#roadmap). |

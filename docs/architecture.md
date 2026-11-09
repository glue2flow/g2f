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
items arriving on its incoming arrows. Nodes with several outputs (e.g. `Condition` → `true`/`false`)
route items to a specific handle; a node that receives no items is **skipped**.

Each execution stores per-node status, duration and output (capped at
`G2F_MAX_STORED_ITEMS` items per output), which the editor shows below the canvas.

The engine runs acyclic graphs only. Loops are built with
[Execute Workflow](nodes.md#execute-workflow) (per item or per batch).

## Scaling and replicas

Run as many replicas as you like against the same PostgreSQL database.

| Part | Several replicas |
| --- | --- |
| Schedules | Every replica runs the scheduler; each firing is claimed once in PostgreSQL (`trigger_firings`). |
| Webhooks, form submissions, manual runs | Any replica can receive them; the execution runs on that replica. |
| Wait nodes | Every replica polls for due waits; each resume is claimed atomically. |
| Polling triggers | Checks happen at interval-aligned times; each is claimed once in `trigger_firings`. Trigger cursors and the last check error live in `workflow_state`. |
| Data Store, Remove Duplicates | Values live in PostgreSQL (`workflow_state`); increments and "seen" checks are single atomic statements. |
| Running executions | Survive the replica that runs them, see below. |
| Imported plugins | Stored in PostgreSQL and reloaded by every replica (`G2F_PLUGIN_SYNC`). |

### Executions survive restarts and crashes

Every instance registers in `engine_instances` and sends a heartbeat every
`G2F_INSTANCE_TIMEOUT / 3`. A running execution belongs to the instance that runs it, and
after every node the engine saves a **checkpoint**: the steps so far and the run state.

- **Crash** (kill, OOM, lost node): when an instance has sent no heartbeat for
  `G2F_INSTANCE_TIMEOUT` (default 30 s), another instance, or the same one after its restart,
  takes its executions over and continues each one after its last completed node.
- **Graceful shutdown** (SIGTERM, rolling deploy): running executions get
  `G2F_SHUTDOWN_GRACE` (default 15 s) to finish. Unfinished ones are handed back and taken
  over by another instance within seconds.
- **Never continued twice:** takeovers are claimed atomically (`FOR UPDATE SKIP LOCKED`),
  and every write is fenced by owner. An instance that was cut off and comes back cannot
  overwrite or continue a run that another instance took over.

What to expect from a takeover:

- Completed nodes never run again.
- The node that was running when the instance stopped **runs again** (at-least-once). Its
  step log says so, and the run shows *continued after a server stop* in the editor. Make
  side effects idempotent where duplicates hurt (e.g. pass an idempotency key to an API).
- Sub-workflow executions are not continued; the calling workflow re-runs its Execute
  Workflow node, which starts a fresh one.
- An execution that was taken over more than 3 times fails, so a run that crashes every
  instance (e.g. by exhausting memory) cannot take the cluster down.
- Executions started by versions without checkpoints (before this feature) are failed with
  *interrupted* once they are older than `G2F_EXECUTION_TIMEOUT`, so a rolling upgrade does
  not touch runs that old instances are still executing.

Checkpoints cost one database write per node. Webhook calls that wait for the result
(`respond: lastNode`) lose their HTTP connection when their instance stops; the run itself
still completes on another instance.

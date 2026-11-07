# g8g

An open-source, self-hosted workflow automation tool in the spirit of n8n / Power Automate:
a visual editor with boxes and arrows, a small Go engine, Keycloak authentication, and
workflows that can be private, shared with groups, or available to the whole organization.

- **Engine:** Go, single static binary, ~25 MB container image, PostgreSQL for state
- **Editor:** React + [React Flow](https://reactflow.dev)
- **Auth:** Keycloak / any OIDC provider (PKCE in the browser, JWT validation in the API)
- **Extensible:** plugins as YAML manifests (HTTP APIs) or sandboxed WebAssembly modules
  (Go, JavaScript, Rust, …), importable at runtime
- **Secrets:** per-node secrets and per-organization plugin settings, encrypted at rest, write-only

## Quick start (Docker)

```bash
docker compose up --build
```

| URL | What | Login |
| --- | --- | --- |
| http://localhost:8080 | g8g editor | `alice`/`alice` (org admin + plugin admin, /engineering/backend), `bob`/`bob` (/engineering/frontend), `carol`/`carol` (/sales), `dave`/`dave` (other org) |
| http://localhost:8081 | Keycloak admin | `admin`/`admin` |

The compose file is a demo: default passwords, Keycloak in dev mode, plain HTTP.

## Local development

```bash
make dev-db       # PostgreSQL in Docker on :5432
make dev-server   # Go API on :8080 with auth disabled (G8G_AUTH_MODE=none)
make dev-web      # Vite dev server on :5173, proxies /api and /webhook to :8080
make test         # go vet, go test, TypeScript typecheck
make plugin-sample   # build examples/plugins/telegram/telegram.zip
```

## Architecture

```
cmd/g8g/             entrypoint (server + `g8g healthcheck`)
internal/config/     environment configuration
internal/auth/       OIDC token validation, User (groups, org, admin role)
internal/workflow/   workflow model, graph validation, sharing / roles
internal/expression/ {{ expr }} templates in node parameters
internal/nodes/      node registry, built-in nodes, manifest format, HTTP modules
internal/plugin/     plugin import/sync, WebAssembly runtime (Extism + wazero)
internal/secrets/    AES-GCM encryption, node secrets, plugin configuration
internal/engine/     executes graphs, records executions
internal/trigger/    scheduler (cron / one-time) and webhook endpoint
internal/store/      PostgreSQL persistence + embedded migrations
internal/api/        REST API, SPA hosting
modules/             plugins loaded at startup (*.yaml, *.zip, folders)
examples/plugins/    sample WebAssembly plugin (Telegram, in Go and JS)
web/                 React editor
deploy/              Keycloak realm + Postgres init for docker-compose
```

### Execution model

A run starts at one trigger node. Every node reachable from it runs once, in topological
order. Data flows as a list of JSON **items**; a node's input is the concatenation of all
items arriving on its incoming arrows. Nodes with several outputs (e.g. `If` → `true`/`false`)
route items to a specific handle; a node that receives no items is **skipped**.

Each execution stores per-node status, duration and output (capped at
`G8G_MAX_STORED_ITEMS` items per output), which the editor shows below the canvas.

### Built-in nodes

| Category | Nodes |
| --- | --- |
| Triggers | Manual, Schedule, Webhook |
| Core | HTTP Request, Set Fields, If, Transform Items, Log |
| Logic | **And**, **Or**, **Xor** |

**Logic gates** combine two branches. Each gate has the inputs **A** and **B** and the outputs
`true` / `false`. An input is *true* when it received at least one item matching its
condition (default `{{ true }}`, i.e. "received anything"). Example: `{{ json.status == 'ok' }}`.

| Gate | `true` when | items on `true` |
| --- | --- | --- |
| And | A and B | items of A and B |
| Or | A or B | items of the true inputs |
| Xor | exactly one of A, B | items of the true input |

All received items go to `false` otherwise. Because a node runs only after all its
predecessors, *And* also works as a join: "continue when both branches produced data".

### Triggers

Every workflow starts at a trigger node. A workflow may have several triggers.

| Trigger | Fires when | Item emitted |
| --- | --- | --- |
| **Manual** | a user with role `runner`+ clicks ▶ Run, or `POST /api/workflows/{id}/run` | the node's test payload (or `items` from the request body) |
| **Schedule** | the workflow is **active** and the time is due | `{firedAt, scheduledAt, timezone}` |
| **Webhook** | the workflow is **active** and `/webhook/{workflowId}/{nodeId}` is called | `{method, headers, query, body, receivedAt}` |

Any trigger can also be run manually from the editor (select it, then ▶ Run). Webhook and
schedule triggers then use their test payload, so a flow can be tested before activation.

**Schedule**
- *Recurring*: 5-field cron (`*/15 * * * *`), or descriptors like `@hourly`, `@daily`, `@every 30m`. At most once per minute.
- *Once*: a local date-time (`2026-12-24T18:00`).
- Each schedule has an IANA timezone (`Europe/Berlin`), so it follows daylight saving time.
- Each firing is claimed in PostgreSQL (`trigger_firings`) under a key derived from the scheduled time.
  With several replicas, every replica runs the scheduler and exactly one executes each firing.
  A one-time schedule never runs twice, even across restarts.
- Missed recurring runs (server down) are skipped. A one-time run missed by less than
  `G8G_SCHEDULE_CATCHUP` (default 1h) runs as soon as the server is back.

**Webhook**
- Method: `POST` (default), `GET`, `PUT`, `PATCH`, `DELETE` or `ANY`.
- Authentication:
  - `none`
  - `token`: shared secret in a header (default `X-G8G-Token`, `Bearer ` prefix allowed)
  - `hmac`: HMAC-SHA256 signature of the raw body in a header (default `X-Signature-256`,
    value `sha256=<hex>`). Set the header to `X-Hub-Signature-256` for GitHub webhooks.
- The secret header and cookies are removed before the request becomes workflow data.
- Response:
  - `immediately`: `202 {"executionId": ...}`
  - `lastNode`: waits up to `G8G_WEBHOOK_MAX_WAIT` and returns the output of the last node
    (one item as an object, otherwise an array). Errors return HTTP 500.
- JSON and form bodies are parsed; anything else arrives as a string. Limit 5 MiB.
- The editor shows the URL and a ready-to-use `curl` command for the selected webhook node.

```bash
curl -X POST http://localhost:8080/webhook/<workflow-id>/<node-id> \
  -H 'X-G8G-Token: s3cret' -H 'Content-Type: application/json' -d '{"name":"Ada"}'
```

### Expressions

Any parameter can contain `{{ ... }}` expressions in the [expr language](https://expr-lang.org/docs/language-definition):

| Variable | Meaning |
| --- | --- |
| `json` | the current item |
| `items` | all input items of the node |
| `nodes['Node name']` | output items of an earlier node |
| `trigger` | the first item emitted by the trigger |
| `workflow.id`, `workflow.name`, `execution.id` | run metadata |
| `params` | resolved node parameters (YAML modules only) |

Member access is null-safe: `{{ trigger.body.name }}` is `nil` when `body` is missing, so
`{{ trigger.body.name ?? 'anonymous' }}` works as a default.
A parameter that is exactly one expression keeps its type (`{{ json.count + 1 }}` → number);
mixed text yields a string. Inside `map`/`filter`, wrap object literals in parentheses:
`{{ map(items, ({id: .id})) }}`.

## Sharing model

Each workflow has an **owner**, an **organization** and a **scope**:

| Scope | Who can access |
| --- | --- |
| `private` | owner only |
| `group` | owner + members of the listed Keycloak groups (subgroups included: a share for `/engineering` also covers `/engineering/backend`) |
| `org` | everyone in the organization with the default `orgRole`; group shares can grant more |

Roles: `viewer` (see workflow and runs) < `runner` (+ run manually) < `editor` (+ edit, activate)
< `owner` (+ delete, change sharing). Users with the realm role `g8g-admin` are owners of every
workflow in their organization. Workflows are never visible across organizations.
Secret parameters are never returned by the API, whatever the role (see [Secrets](#secrets)).

### Keycloak setup

g8g reads these claims from the access token (claim names are configurable):

| Claim | Source in Keycloak | Env var |
| --- | --- | --- |
| `groups` | *Group Membership* mapper with **full group path** on | `G8G_OIDC_GROUPS_CLAIM` |
| `org` | *User Attribute* mapper for attribute `org`, or the `organization` claim of Keycloak Organizations (both formats supported) | `G8G_OIDC_ORG_CLAIM` |
| `realm_access.roles` | realm role `g8g-admin` | `G8G_OIDC_ADMIN_ROLE` |

`deploy/keycloak/realm-g8g.json` has a complete example: a public client `g8g` with PKCE,
both mappers, groups, and demo users.

## Secrets

Secret values never stay in the workflow JSON:

- **Per-node secrets:** node parameters of type `secret`, e.g. an API token per user or per
  node. On save they are moved into `node_secrets`. The API returns
  `{"$secret": "stored"}` instead of the value; sending that placeholder back keeps the
  value, and an empty value deletes it. The engine decrypts them right before the node runs.
- **Plugin configuration:** global settings declared by a plugin (`config:` in its manifest,
  e.g. an OAuth client secret or a default account). One value per organization, set by an
  organization admin on the *Plugins* page (`GET/PUT /api/plugins/{module}/config`). Nodes
  read them as `config.<name>`. Secret entries are write-only.

Both are encrypted with AES-256-GCM using `G8G_SECRET_KEY`. Each ciphertext is bound to its
location (workflow/node/parameter, or org/module/key), so it cannot be copied into another
row. Error messages are scrubbed of known secret values before they are stored.

Key rotation: set a new `G8G_SECRET_KEY` and put the old one into `G8G_SECRET_KEYS_OLD`.
Old values stay readable; values are re-encrypted with the new key when they are saved again.

```bash
openssl rand -base64 32   # generate a key
```

## Plugins

A plugin (module) is a set of nodes described by a manifest. There are two runtimes:

| Runtime | Implementation | Use for |
| --- | --- | --- |
| `http` | YAML only: each node maps its parameters onto one HTTP request | thin REST wrappers |
| `wasm` | WebAssembly module (Extism PDK), sandboxed | any logic, any language (Go, JS/TS, Rust, …) |

Install plugins in one of two ways:

- **At startup:** put them into `G8G_MODULES_DIR`: `*.yaml`, `*.zip` bundles, or folders with
  `plugin.yaml` (+ `.wasm`). These are read-only in the UI.
- **At runtime:** import them on the *Plugins* page or with `POST /api/plugins` (multipart
  field `file`), as a user with the `g8g-plugin-admin` realm role. Imported plugins are
  stored in PostgreSQL and loaded by all replicas. Re-importing a module upgrades it, and
  `DELETE /api/plugins/{module}` uninstalls it.

WASM plugins run in a fresh instance per node execution with no file system access and a
memory and time limit. They can make HTTP calls only to the manifest's `allowedHosts`.
Plugins are installation-wide, which is why importing them needs a dedicated role separate
from organization admins.

See **[examples/plugins/telegram](examples/plugins/telegram)** for a complete WASM plugin in
Go and JavaScript, the JSON calling convention and build instructions.

### Manifest reference

```yaml
module: telegram              # unique id, prefix of node types (telegram.sendMessage)
name: Telegram
version: 1.0.0
runtime: wasm                 # http (default) | wasm
wasm: plugin.wasm             # wasm: file inside the zip
allowedHosts: [api.telegram.org]
timeout: 20s                  # wasm: per node run (default 30s)
memoryMB: 32                  # wasm: default 64

config:                       # global settings per organization (admin, encrypted)
  - { name: botToken, label: Default bot token, type: secret }
params:                       # parameters shared by all nodes of the module
  - { name: botToken, label: Bot token, type: secret }   # per-node secret
nodes:
  - type: sendMessage
    name: Send Message
    params:
      - { name: chatId, required: true }
    inputs: [main]            # optional, default [main]
    outputs: [main]           # optional, default [main]
```

## Writing HTTP modules

Most integrations are thin wrappers around a REST API. They can be written as YAML in
`modules/` (loaded at startup, see [`modules/github.yaml`](modules/github.yaml)):

```yaml
module: github
name: GitHub
baseUrl: https://api.github.com
params:                       # shared by all nodes of the module
  - { name: token, label: Access token, type: secret }
headers:
  Authorization: "{{ params.token != '' ? 'Bearer ' + params.token : '' }}"   # empty headers are dropped
nodes:
  - type: repo.get            # becomes github.repo.get
    name: Get Repository
    params:
      - { name: owner, required: true }
      - { name: repo, required: true }
    request:
      method: GET
      path: /repos/{{ params.owner }}/{{ params.repo }}
    output: "{{ response.body }}"   # optional; arrays become one item per element
```

Inside the request templates, `params` holds the node's resolved parameters (secrets
decrypted) and `config` holds the module's organization settings.

Parameter types: `string`, `text`, `number`, `boolean`, `select` (with `options`), `json`, `secret`, `datetime`.
`showIf: {auth: [token, hmac]}` shows a field only for certain values of another parameter.

For logic that does not fit a single HTTP request, write a WASM plugin (see above), or
implement the `nodes.Node` interface in Go (see `internal/nodes/builtin.go`) and register it
in `RegisterBuiltins`.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `G8G_LISTEN` | `:8080` | listen address |
| `G8G_DATABASE_URL` | `postgres://g8g:g8g@localhost:5432/g8g?sslmode=disable` | PostgreSQL URL |
| `G8G_PUBLIC_URL` | `http://localhost:8080` | external URL (used for webhook URLs) |
| `G8G_SECRET_KEY` | – (required) | base64 32-byte key encrypting secrets |
| `G8G_SECRET_KEYS_OLD` | – | previous keys (comma separated), for rotation |
| `G8G_AUTH_MODE` | `oidc` | `oidc` or `none` (development only) |
| `G8G_OIDC_ISSUER` | – | expected token issuer, e.g. `https://sso.example.com/realms/g8g` |
| `G8G_OIDC_JWKS_URL` | `<issuer>/protocol/openid-connect/certs` | where to fetch signing keys |
| `G8G_OIDC_CLIENT_ID` | `g8g` | client id (checked against `azp`/`aud`) |
| `G8G_OIDC_GROUPS_CLAIM` | `groups` | |
| `G8G_OIDC_ORG_CLAIM` | `org` | |
| `G8G_OIDC_ADMIN_ROLE` | `g8g-admin` | organization admin |
| `G8G_OIDC_PLUGIN_ADMIN_ROLE` | `g8g-plugin-admin` | may import plugins (installation-wide) |
| `G8G_DEFAULT_ORG` | `default` | org for users without an org claim |
| `G8G_WORKERS` | `16` | max concurrent executions |
| `G8G_EXECUTION_TIMEOUT` | `5m` | per execution |
| `G8G_MAX_STORED_ITEMS` | `100` | items stored per node output |
| `G8G_SCHEDULE_CATCHUP` | `1h` | how late a missed one-time schedule may still run |
| `G8G_WEBHOOK_MAX_WAIT` | `60s` | max wait for webhooks that respond with the result |
| `G8G_STATIC_DIR` | `./web/dist` | built editor |
| `G8G_MODULES_DIR` | `./modules` | plugins loaded at startup |
| `G8G_PLUGIN_SYNC` | `30s` | how often imported plugins are reloaded from the database |

## Roadmap

- Reusable credentials (named, shareable like workflows) in addition to per-node secrets
- Plugin signing and a plugin catalog
- Code node (JavaScript via goja / QuickJS-WASM)
- Horizontal scaling: run executions through a Postgres-backed job queue (schedules are
  already replica-safe; the restart recovery of running executions still assumes one instance)
- Webhook rate limiting and a "test URL" that works while a workflow is inactive
- Workflow version history and import/export
- SSRF protection for HTTP nodes (block private networks by default)

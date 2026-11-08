# glue2flow

**glue2flow** (short: **g2f**) is an open-source, self-hosted workflow automation tool in the spirit of n8n / Power Automate:
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
| http://localhost:8080 | glue2flow editor | `alice`/`alice` (org admin + plugin admin, /engineering/backend), `bob`/`bob` (/engineering/frontend), `carol`/`carol` (/sales), `dave`/`dave` (other org) |
| http://localhost:8081 | Keycloak admin | `admin`/`admin` |

The compose file is a demo: default passwords, Keycloak in dev mode, plain HTTP.

## Local development

```bash
make dev-db       # PostgreSQL in Docker on :5432
make dev-server   # Go API on :8080 with auth disabled (G2F_AUTH_MODE=none)
make dev-web      # Vite dev server on :5173, proxies /api and /webhook to :8080
make test         # go vet, go test, TypeScript typecheck
make plugin-sample   # build examples/plugins/telegram/telegram.zip
```

## Architecture

```
cmd/g2f/             entrypoint (server + `g2f healthcheck`)
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
examples/plugins/    sample WebAssembly plugins (Telegram in Go and JS, Mistral AI in Go)
web/                 React editor
deploy/              Keycloak realm + Postgres init for docker-compose
```

### Execution model

A run starts at one trigger node. Every node reachable from it runs once, in topological
order. Data flows as a list of JSON **items**; a node's input is the concatenation of all
items arriving on its incoming arrows. Nodes with several outputs (e.g. `If` → `true`/`false`)
route items to a specific handle; a node that receives no items is **skipped**.

Each execution stores per-node status, duration and output (capped at
`G2F_MAX_STORED_ITEMS` items per output), which the editor shows below the canvas.

### Built-in nodes

| Category | Nodes |
| --- | --- |
| Triggers | Manual, Schedule, Webhook, **Called by Workflow**, **Error Trigger** |
| Core | HTTP Request, Set Fields, If, **Switch**, **Merge**, **Code**, **Wait**, Transform Items, **Log** |
| Flow | **Execute Workflow** (incl. loops over items / batches) |
| Logic | **And**, **Or**, **Xor** |

**Switch** routes each item by value: `value: {{ json.severity }}`, `cases: critical, warning`
creates the outputs `critical`, `warning` and `default`.

**Merge** combines inputs A and B:
- *append*: A, then B;
- *join by key*: `{{ json.id }}` on both sides, keeping inner, left or outer matches;
- *pair by position*.

Fields of B win on conflicts.

**Code** runs JavaScript in an embedded, sandboxed interpreter ([goja](https://github.com/dop251/goja)).
There is no network, file system, modules or timers, every run uses a fresh VM, and runs stop
after a timeout (default 10 s).
- *Once for all items*: use `items`, return an array.
- *Once per item*: use `item` and `index`, return an object, or `null` to drop the item.

`nodes`, `trigger`, `workflow`, `execution` and `console.log()` (also `info`, `warn`, `error`,
`debug`) are available. Console output goes to the run's execution log. Administrators
can turn the node off with `G2F_DISABLE_CODE_NODE=true`. Memory use of scripts is not limited.

```js
return items.filter(i => i.total > 100).map(i => ({ id: i.id, vip: true }));
```

**Wait** pauses an execution and stores its complete state in PostgreSQL, so waits of
minutes, days or weeks survive restarts and deployments. Three modes:
- *after an interval*;
- *at a specific time* (with timezone);
- *when the resume URL is called*: `{{ execution.resumeUrl }}` can be used in any node before
  the Wait, e.g. in an approval message. Calling it with any HTTP method continues the run, and
  the request (`query`, `body`, `headers`) becomes the Wait node's output.

Unanswered webhook waits continue on the `timeout` output after the configured hours. Resume
URLs are HMAC-signed with a key derived from `G2F_SECRET_KEY`. Waiting runs show a *Cancel*
button, and the execution timeout only counts active time. Every replica polls for due waits;
each resume is claimed atomically, so it happens exactly once.

```
Expense ─► Ask manager (Slack: "{{ execution.resumeUrl }}?decision=approve") ─► Wait ─► Switch(json.query.decision)
                                                                                   └─timeout─► Escalate
```

**Execute Workflow** runs another workflow that starts with a *Called by Workflow* trigger, and
continues with that workflow's last output. Modes:
- *once* with all items;
- *per item*;
- *per batch of N*, with an optional pause between runs.

The per-item and batch modes are how glue2flow loops: the engine runs acyclic graphs, and a loop body
becomes a reusable sub-workflow. Each call is its own execution (`triggered by workflow: …`).
Calls stay within the organization and nest at most 5 levels deep. Saving requires the
`runner` role on the called workflow. Wait nodes are not allowed in called workflows.

**Error Trigger** starts a workflow when a run fails. It can watch the workflow it belongs to
(an in-workflow error handler) or selected workflows (watching others requires viewer access
and an active error workflow). It emits `workflow` (with editor URL), `execution`,
`failedNode` and `error`. Manual test runs are ignored unless enabled, and failures of error
workflows never trigger further error workflows.

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

**Log** writes its *Message* (multi-line, with `{{ expressions }}`) once per item at the chosen
level (debug, info, warning, error) and passes the items on unchanged. Messages appear in the
editor under the run: the **Log** tab of the Executions panel shows all messages of a run, and
each node's tab shows its own. They also go to the server log with the workflow and execution
IDs. Known secret values are masked. A node keeps at most 500 messages of up to 8 KB each per run.

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
  `G2F_SCHEDULE_CATCHUP` (default 1h) runs as soon as the server is back.

**Webhook**
- Method: `POST` (default), `GET`, `PUT`, `PATCH`, `DELETE` or `ANY`.
- Authentication:
  - `none`
  - `token`: shared secret in a header (default `X-G2F-Token`, `Bearer ` prefix allowed)
  - `hmac`: HMAC-SHA256 signature of the raw body in a header (default `X-Signature-256`,
    value `sha256=<hex>`). Set the header to `X-Hub-Signature-256` for GitHub webhooks.
- The secret header and cookies are removed before the request becomes workflow data.
- Response:
  - `immediately`: `202 {"executionId": ...}`
  - `lastNode`: waits up to `G2F_WEBHOOK_MAX_WAIT` and returns the output of the last node
    (one item as an object, otherwise an array). Errors return HTTP 500.
- JSON and form bodies are parsed; anything else arrives as a string. Limit 5 MiB.
- The editor shows the URL and a ready-to-use `curl` command for the selected webhook node.

```bash
curl -X POST http://localhost:8080/webhook/<workflow-id>/<node-id> \
  -H 'X-G2F-Token: s3cret' -H 'Content-Type: application/json' -d '{"name":"Ada"}'
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

**Dynamic content (⚡).** Every field that accepts expressions has a ⚡ button. It lists
the fields of all upstream nodes (closest first), the trigger, the plugin's `vars` and
the workflow metadata. Clicking a field inserts the expression at the cursor: `{{ json.text }}`
for the directly connected node, `{{ nodes['HTTP Request'][0].body.id }}` for earlier ones.
Inside an open `{{ }}` only the path is inserted. In JSON fields it is quoted, and in Code
fields it is inserted without braces, with `item` or `items[0]` in place of `json`.
The fields come from the latest run (with sample values) and from each node type's
`outputSchema`, so nodes that have not run yet are listed too.

## Sharing model

Each workflow has an **owner**, an **organization** and a **scope**:

| Scope | Who can access |
| --- | --- |
| `private` | owner only |
| `group` | owner + members of the listed Keycloak groups (subgroups included: a share for `/engineering` also covers `/engineering/backend`) |
| `org` | everyone in the organization with the default `orgRole`; group shares can grant more |

Roles: `viewer` (see workflow and runs) < `runner` (+ run manually) < `editor` (+ edit, activate)
< `owner` (+ delete, change sharing). Users with the realm role `g2f-admin` are owners of every
workflow in their organization. Workflows are never visible across organizations.
Secret parameters are never returned by the API, whatever the role (see [Secrets](#secrets)).

### Keycloak setup

glue2flow reads these claims from the access token (claim names are configurable):

| Claim | Source in Keycloak | Env var |
| --- | --- | --- |
| `groups` | *Group Membership* mapper with **full group path** on | `G2F_OIDC_GROUPS_CLAIM` |
| `org` | *User Attribute* mapper for attribute `org`, or the `organization` claim of Keycloak Organizations (both formats supported) | `G2F_OIDC_ORG_CLAIM` |
| `realm_access.roles` | realm role `g2f-admin` | `G2F_OIDC_ADMIN_ROLE` |

`deploy/keycloak/realm-g2f.json` has a complete example: a public client `g2f` with PKCE,
both mappers, groups, and demo users.

## Secrets

Secret values never stay in the workflow JSON:

- **Per-node secrets:** node parameters of type `secret`, e.g. an API token per user or per
  node. On save they are moved into `node_secrets`. The API returns
  `{"$secret": "stored"}` instead of the value; sending that placeholder back keeps the
  value, and an empty value deletes it. The engine decrypts them right before the node runs.
- **Plugin settings:** declared in the plugin's manifest and set once per organization by
  an organization admin on the *Plugins* page (`GET/PUT /api/plugins/{module}/config`).
  There are two kinds:

  | Kind | Manifest section | Readable by | Typical content |
  | --- | --- | --- | --- |
  | **config** | `config:` | the plugin only: WASM code (`config`) and the manifest's request templates (`{{ config.x }}`) | API tokens, client secrets, base URLs |
  | **vars** | `vars:` | workflow editors, in node fields: `{{ vars.x }}` | default channel or chat, team name |

  Config never reaches expressions in node fields. A field such as a message text cannot
  contain the bot token: saving a workflow whose node fields reference `config` fails, and at
  run time `config` does not exist in their scope. The plugin decides where config values go
  (URL, `Authorization` header, `X-…` headers, request body). Vars cannot be secrets.
  Secret config entries are write-only.

All of these are encrypted with AES-256-GCM using `G2F_SECRET_KEY`. Each ciphertext is bound to its
location (workflow/node/parameter, or org/module/key), so it cannot be copied into another
row. Error messages are scrubbed of known secret values before they are stored.

Key rotation: set a new `G2F_SECRET_KEY` and put the old one into `G2F_SECRET_KEYS_OLD`.
Old values stay readable; values are re-encrypted with the new key when they are saved again.

```bash
openssl rand -base64 32   # generate a key
```

## Connected accounts (OAuth2)

Plugins can let users connect their own accounts at a provider, such as Google, Microsoft or
HubSpot. The host runs the whole OAuth2 flow and stores the tokens, and plugins only receive a
valid access token while a node runs.

1. **Admin, once per organization:** register an OAuth app at the provider with the redirect
   URI `<G2F_PUBLIC_URL>/oauth/callback` (shown in the plugin's *Settings* dialog), then enter
   the client ID and secret in *Plugins → Settings*. They are plugin `config`, encrypted.
2. **User:** *Connections → Connect …*, or *＋ Connect* next to a node's **Account** field. A
   popup opens the provider's consent page and returns to glue2flow.
3. **Workflows:** every node of an OAuth2 plugin has an **Account** field. Workflows store only
   the connection id.

| Topic | Behaviour |
| --- | --- |
| Storage | Access and refresh tokens are encrypted (AES-256-GCM) and never returned by the API. |
| Refresh | Tokens are refreshed shortly before they expire. The connection row is locked during a refresh, so parallel runs and replicas refresh only once; providers that rotate refresh tokens keep working. |
| Flow security | Authorization code flow with PKCE (`pkce: true`). The `state` is an encrypted, 10-minute blob bound to the user, plugin and PKCE verifier, so no server-side session is needed. |
| Token delivery | HTTP plugins: added as `Authorization` header (or `{{ oauth.accessToken }}` in manifest templates), **only for `apiHosts`**. Requests to any other host are refused. WASM plugins: `input.oauth.accessToken`. Node fields cannot read the token. |
| Sharing | A connection belongs to whoever connected it and is *Only me*, shared with *Groups*, or the *Organization*. Selecting an account in a node requires access to it. Co-editors can still save workflows using someone else's account if they do not change that selection. Only the owner or an org admin can rename, share or remove it. |
| Errors | Known token values are scrubbed from error messages. |

`modules/google-sheets.yaml` (Append Row, Read Rows) is a complete example. It stays inactive
until an admin enters the Google OAuth client ID and secret.

```yaml
config:                          # OAuth app credentials (admin, encrypted)
  - { name: clientId }
  - { name: clientSecret, type: secret }
oauth2:
  authorizeUrl: https://accounts.google.com/o/oauth2/v2/auth
  tokenUrl: https://oauth2.googleapis.com/token
  scopes: [https://www.googleapis.com/auth/spreadsheets, openid, email]
  pkce: true
  authorizeParams: { access_type: offline, prompt: consent }   # provider-specific extras
  userInfo: { url: https://openidconnect.googleapis.com/v1/userinfo, field: email }  # account label
  apiHosts: [sheets.googleapis.com]   # default: host of baseUrl
  # tokenAuth: basic                  # client auth at the token endpoint (default: body)
  # clientIdConfig / clientSecretConfig: names of the config entries (default clientId / clientSecret)
```

## Plugins

A plugin (module) is a set of nodes described by a manifest. There are two runtimes:

| Runtime | Implementation | Use for |
| --- | --- | --- |
| `http` | YAML only: each node maps its parameters onto one HTTP request | thin REST wrappers |
| `wasm` | WebAssembly module (Extism PDK), sandboxed | any logic, any language (Go, JS/TS, Rust, …) |

Install plugins in one of two ways:

- **At startup:** put them into `G2F_MODULES_DIR`: `*.yaml`, `*.zip` bundles, or folders with
  `plugin.yaml` (+ `.wasm`). These are read-only in the UI.
- **At runtime:** import them on the *Plugins* page or with `POST /api/plugins` (multipart
  field `file`), as a user with the `g2f-plugin-admin` realm role. Imported plugins are
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

config:                       # plugin-only settings per organization (admin, encrypted)
  - { name: botToken, label: Default bot token, type: secret }
  - { name: apiBaseUrl, default: https://api.telegram.org }
vars:                         # per-organization values usable in node fields as {{ vars.x }}
  - { name: defaultChatId, label: Default chat ID }
params:                       # parameters shared by all nodes of the module
  - { name: botToken, label: Bot token, type: secret }   # per-node secret
nodes:
  - type: sendMessage
    name: Send Message
    params:
      - { name: chatId, required: true }
    inputs: [main]            # optional, default [main]
    outputs: [main]           # optional, default [main]
    outputSchema:             # optional: fields of the emitted items, for the ⚡ picker
      fields:
        - { path: message_id, type: number }
        - { path: chat.id, type: number, description: Chat the message was sent to }
      # passthrough: true     # items keep the input's fields (plus the ones above)
      # fromParam: fields     # a parameter whose object value describes the items
      # dynamic: true         # shape not known in advance; only run data is shown
```

`type` is `string`, `number`, `boolean`, `object`, `array` or `any`. Paths use dots for
nesting; parent objects need not be listed.

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

Inside the request templates (`url`, `path`, `headers`, `query`, `body`, `output`), `params`
holds the node's resolved parameters (secrets decrypted) and `config` the module's
plugin-only settings. For example:
`Authorization: "Bearer {{ config.apiToken }}"`. Node fields only see `vars`.

Parameter types: `string`, `text`, `number`, `boolean`, `select` (with `options`), `json`, `secret`, `datetime`.
`showIf: {auth: [token, hmac]}` shows a field only for certain values of another parameter.

For logic that does not fit a single HTTP request, write a WASM plugin (see above), or
implement the `nodes.Node` interface in Go (see `internal/nodes/builtin.go`) and register it
in `RegisterBuiltins`.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `G2F_LISTEN` | `:8080` | listen address |
| `G2F_DATABASE_URL` | `postgres://g2f:g2f@localhost:5432/g2f?sslmode=disable` | PostgreSQL URL |
| `G2F_PUBLIC_URL` | `http://localhost:8080` | external URL (used for webhook URLs) |
| `G2F_SECRET_KEY` | – (required) | base64 32-byte key encrypting secrets |
| `G2F_SECRET_KEYS_OLD` | – | previous keys (comma separated), for rotation |
| `G2F_AUTH_MODE` | `oidc` | `oidc` or `none` (development only) |
| `G2F_OIDC_ISSUER` | – | expected token issuer, e.g. `https://sso.example.com/realms/g2f` |
| `G2F_OIDC_JWKS_URL` | `<issuer>/protocol/openid-connect/certs` | where to fetch signing keys |
| `G2F_OIDC_CLIENT_ID` | `g2f` | client id (checked against `azp`/`aud`) |
| `G2F_OIDC_GROUPS_CLAIM` | `groups` | |
| `G2F_OIDC_ORG_CLAIM` | `org` | |
| `G2F_OIDC_ADMIN_ROLE` | `g2f-admin` | organization admin |
| `G2F_OIDC_PLUGIN_ADMIN_ROLE` | `g2f-plugin-admin` | may import plugins (installation-wide) |
| `G2F_DEFAULT_ORG` | `default` | org for users without an org claim |
| `G2F_WORKERS` | `16` | max concurrent executions |
| `G2F_EXECUTION_TIMEOUT` | `5m` | per execution (active time; waiting does not count) |
| `G2F_MAX_STORED_ITEMS` | `100` | items stored per node output |
| `G2F_SCHEDULE_CATCHUP` | `1h` | how late a missed one-time schedule may still run |
| `G2F_WEBHOOK_MAX_WAIT` | `60s` | max wait for webhooks that respond with the result |
| `G2F_STATIC_DIR` | `./web/dist` | built editor |
| `G2F_MODULES_DIR` | `./modules` | plugins loaded at startup |
| `G2F_PLUGIN_SYNC` | `30s` | how often imported plugins are reloaded from the database |
| `G2F_DISABLE_CODE_NODE` | `false` | turn off the JavaScript Code node |

## Roadmap

- Reusable API-key credentials (like connections, for non-OAuth services)
- Plugin signing and a plugin catalog
- More OAuth2 integration packs (Gmail, Microsoft 365, HubSpot, Salesforce)
- Built-in protocol nodes: SMTP/IMAP, Postgres/MySQL, SFTP
- Memory limits for the Code node (e.g. QuickJS in WASM)
- Horizontal scaling: run executions through a Postgres-backed job queue (schedules are
  already replica-safe; the restart recovery of running executions still assumes one instance)
- Webhook rate limiting and a "test URL" that works while a workflow is inactive
- Workflow version history and import/export
- SSRF protection for HTTP nodes (block private networks by default)

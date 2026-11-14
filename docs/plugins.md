[Docs](README.md) › Plugins

# Plugins

A plugin (module) is a set of nodes described by a manifest. There are three runtimes:

| Runtime | Implementation | Use for |
| --- | --- | --- |
| `http` | YAML only: each node maps its parameters onto one HTTP request | thin REST wrappers, see [HTTP modules](http-modules.md) |
| `wasm` | WebAssembly module (Extism PDK), sandboxed | any logic, any language (Go, JS/TS, Rust, …) |
| `go` | Go code compiled into the server, declared by a YAML manifest | protocols the sandbox cannot speak (SMTP, IMAP, databases), see [below](#native-go-modules) |

## Included plugins

| Plugin | Runtime | Nodes | Where |
| --- | --- | --- | --- |
| GitHub | http | Get Repository, List Issues, Create Issue | [`modules/github.yaml`](../modules/github.yaml) |
| Slack | http | Post Message (incoming webhook) | [`modules/slack.yaml`](../modules/slack.yaml) |
| Google Sheets | http + OAuth2 | Append Row, Read Rows | [`modules/google-sheets.yaml`](../modules/google-sheets.yaml) |
| Gmail | http + OAuth2 | New Email (trigger), Send Email, Search Messages, Get Message, Get Attachment, Change Labels, Move to Trash, List Labels | [`modules/gmail.yaml`](../modules/gmail.yaml) |
| Outlook | http + OAuth2 | New Email (trigger), Send Email, List Messages, Get Message, Reply, Move Message, Mark as Read, List Folders | [`modules/outlook.yaml`](../modules/outlook.yaml) |
| Microsoft Teams | http + OAuth2 | Send Channel Message, Reply in Channel Thread, Send Chat Message, List Teams / Channels / Chats | [`modules/ms-teams.yaml`](../modules/ms-teams.yaml) |
| Google Calendar | http + OAuth2 | New or Changed Event (trigger), List Events, Get / Create / Update / Delete Event, List Calendars | [`modules/google-calendar.yaml`](../modules/google-calendar.yaml) |
| Google Drive | http + OAuth2 | List Files, Get File, Create Folder, Copy File, Rename or Move File, Move to Trash, Delete Permanently, Share File, Export Doc as Text | [`modules/google-drive.yaml`](../modules/google-drive.yaml) |
| HubSpot | http + OAuth2 | Get / Search / Create / Update Contact, Get / Search / Create / Update Deal, Create Company, List Owners | [`modules/hubspot.yaml`](../modules/hubspot.yaml) |
| Salesforce | http + OAuth2 | SOQL Query, Get / Create / Update / Upsert / Delete Record of any sObject | [`modules/salesforce.yaml`](../modules/salesforce.yaml) |
| SharePoint | http + OAuth2 | Search Sites, Get Site, List Lists, List / Get / Create / Update / Delete Items, List Document Libraries, List Files and Folders | [`modules/sharepoint.yaml`](../modules/sharepoint.yaml) |
| Email (SMTP/IMAP) | go | Email Received (trigger), Send Email, Read Emails, Move Email, Set Email Flags ([docs](email.md)) | [`modules/email`](../modules/email) |
| Telegram | wasm (Go and JS) | Send Message, Get Bot Info | [`examples/plugins/telegram`](../examples/plugins/telegram) |
| Mistral AI | wasm (Go) | Chat (prompt → answer) | [`examples/plugins/mistral`](../examples/plugins/mistral) |

The `examples/plugins` are built and imported separately (`make plugin-sample`, `make plugin-mistral`).

## Installing

- **At startup:** put them into `G2F_MODULES_DIR`: `*.yaml`, `*.zip` bundles, or folders with
  `plugin.yaml` (+ `.wasm`). These are read-only in the UI.
- **At runtime:** import them on the *Plugins* page or with `POST /api/plugins` (multipart
  field `file`), as a user with the `g2f-plugin-admin` realm role. Imported plugins are
  stored in PostgreSQL and loaded by all replicas. Re-importing a module upgrades it, and
  `DELETE /api/plugins/{module}` uninstalls it.

Plugins are installation-wide, which is why importing them needs a dedicated role separate
from organization admins.

## WebAssembly plugins

WASM plugins run in a fresh instance per node execution with no file system access and a
memory and time limit. They can make HTTP calls only to the manifest's `allowedHosts`, and
every redirect hop must stay within them. Plugin requests are also subject to the
[network access](nodes.md#network-access) rules: hosts in `allowedHosts` that resolve to
private addresses are blocked unless the administrator allows them.

See **[examples/plugins/telegram](../examples/plugins/telegram)** for a complete WASM plugin in
Go and JavaScript, the JSON calling convention and build instructions, and
**[examples/plugins/mistral](../examples/plugins/mistral)** for a compact Go plugin that calls an LLM.

## Native Go modules

WASM plugins can only make HTTP calls. For other protocols a module is written in Go and
compiled into glue2flow, e.g. [`modules/email`](../modules/email):

- `modules/<name>/plugin.yaml` is a normal manifest with `runtime: go`. It declares the
  nodes, parameters, `config`, `vars` and output schemas, so settings, secrets and the editor
  work exactly as for other plugins.
- The Go package in the same folder calls
  `nodes.RegisterNative("<module>", map[string]nodes.ExecFunc{...})` in `init()`, with one
  function per node type. `cmd/g2f/main.go` imports it.
- The module is active only while its folder is in `G2F_MODULES_DIR`, so admins can remove it.
  Go modules cannot be imported at runtime.

## Manifest reference

```yaml
module: telegram              # unique id, prefix of node types (telegram.sendMessage)
name: Telegram
version: 1.0.0
color: "#229ED9"              # tile/border color in the editor
icon: icon.svg                # optional, see "Icons" below
runtime: wasm                 # http (default) | wasm | go
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
```

Config, vars and secret parameters are explained in [Secrets & plugin settings](secrets.md),
OAuth2 (`oauth2:`) in [Connected accounts](connections.md).

### Icons

`icon` shows a logo instead of the colored tile with the plugin's first letter. It may be:

- a file next to the manifest: `icon.svg`, `icons/logo.png` (in a plugin folder, a `.zip`
  bundle, or the modules directory for single-file YAML modules such as
  [`modules/github.yaml`](../modules/github.yaml) → `modules/icons/github.svg`);
- inline SVG markup (`icon: |` followed by `<svg …>…</svg>`), the only option when a bare
  `.yaml` file is imported;
- a base64 `data:` URI.

SVG, PNG and WebP are accepted (detected from the content), at most 32 KB; square images
work best. A node can set its own `icon` to override the plugin's. Icons of imported plugins
are stored with the plugin, so every replica shows them.

The editor shows icons only as images (`<img>`), where browsers never run scripts. SVGs are
checked anyway: scripts, event handlers, embedded documents and references to anything outside
the file are refused.

Without an icon, the node shows the plugin's color with the first letter of its name.
Built-in nodes have their own line icons.

### Polling triggers

A node with `trigger: poll` becomes a [polling trigger](triggers.md#polling-triggers). In an
`http` module, its request is sent on every check, and `pollKey` (default `{{ json.id }}`)
identifies the output items: items whose key was emitted before are dropped. glue2flow adds the
*Check every (minutes)* parameter.

```yaml
  - type: newEmail
    name: New Email
    trigger: poll
    pollKey: "{{ json.id }}"
    request: { method: GET, path: /messages, query: { q: "{{ params.query }}" } }
    output: "{{ reverse(response.body.messages ?? []) }}"   # oldest first
```

In a Go module, the node's function is the check: it runs with `TriggerType` `poll` (or
`manual` for test runs, see `ExecContext.IsTestRun`) and returns the new items, keeping its own
cursor in `ExecContext.State`.

### Parameters

Types: `string`, `text`, `number`, `boolean`, `select` (with `options`), `json`, `secret`,
`datetime`, `code`. `showIf: {auth: [token, hmac]}` shows a field only for certain values of
another parameter. Further keys: `label`, `description`, `placeholder`, `default`, `required`.

### Output schema

`outputSchema` tells the editor which fields a node emits, so the
[⚡ dynamic content picker](expressions.md#dynamic-content-) can offer them before the
workflow has run. After a run, the recorded output is shown as well.

```yaml
outputSchema:
  fields:
    - { path: text, type: string, description: The model's answer }
    - { path: usage.totalTokens, type: number }    # nesting with dots; parents need not be listed
  # passthrough: true          # items keep the input's fields (plus the ones above)
  # passthroughIf: keepExisting  # ...only while this boolean parameter is true
  # fromParam: fields          # a parameter whose object value describes the items
  # dynamic: true              # shape not known in advance; only run data is shown
```

| Key | Meaning |
| --- | --- |
| `fields[].path` | dot path of the field |
| `fields[].type` | `string`, `number`, `boolean`, `object`, `array` or `any` |
| `fields[].description` | shown in the picker |
| `fields[].fromParam` | the keys of this parameter's value become sub-fields (e.g. a test body) |
| `passthrough` | items keep the fields of the input items |
| `passthroughIf` | boolean parameter that turns passthrough on |
| `fromParam` | parameter whose value (object, or list of objects) describes the items |
| `dynamic` | the output depends on code or another workflow |

Schemas are validated when a manifest is loaded: unknown types, invalid paths, duplicate
fields and references to unknown parameters are rejected.

[Docs](README.md) › Plugins

# Plugins

A plugin (module) is a set of nodes described by a manifest. There are two runtimes:

| Runtime | Implementation | Use for |
| --- | --- | --- |
| `http` | YAML only: each node maps its parameters onto one HTTP request | thin REST wrappers, see [HTTP modules](http-modules.md) |
| `wasm` | WebAssembly module (Extism PDK), sandboxed | any logic, any language (Go, JS/TS, Rust, …) |

## Included plugins

| Plugin | Runtime | Nodes | Where |
| --- | --- | --- | --- |
| GitHub | http | Get Repository, List Issues, Create Issue | [`modules/github.yaml`](../modules/github.yaml) |
| Slack | http | Post Message (incoming webhook) | [`modules/slack.yaml`](../modules/slack.yaml) |
| Google Sheets | http + OAuth2 | Append Row, Read Rows | [`modules/google-sheets.yaml`](../modules/google-sheets.yaml) |
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
memory and time limit. They can make HTTP calls only to the manifest's `allowedHosts`.

See **[examples/plugins/telegram](../examples/plugins/telegram)** for a complete WASM plugin in
Go and JavaScript, the JSON calling convention and build instructions, and
**[examples/plugins/mistral](../examples/plugins/mistral)** for a compact Go plugin that calls an LLM.

## Manifest reference

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
```

Config, vars and secret parameters are explained in [Secrets & plugin settings](secrets.md),
OAuth2 (`oauth2:`) in [Connected accounts](connections.md).

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

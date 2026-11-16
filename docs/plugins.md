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
| GitHub | http + credential | Get Repository, List Issues, Create Issue | [`modules/github.yaml`](../modules/github.yaml) |
| Slack | http + credential | Post Message (incoming webhook) | [`modules/slack.yaml`](../modules/slack.yaml) |
| Google Sheets | http + OAuth2 | Append Row, Read Rows | [`modules/google-sheets.yaml`](../modules/google-sheets.yaml) |
| Gmail | http + OAuth2 | New Email (trigger), Send Email, Search Messages, Get Message, Get Attachment, Change Labels, Move to Trash, List Labels | [`modules/gmail.yaml`](../modules/gmail.yaml) |
| Outlook | http + OAuth2 | New Email (trigger), Send Email, List Messages, Get Message, Reply, Move Message, Mark as Read, List Folders | [`modules/outlook.yaml`](../modules/outlook.yaml) |
| Microsoft Teams | http + OAuth2 | Send Channel Message, Reply in Channel Thread, Send Chat Message, List Teams / Channels / Chats | [`modules/ms-teams.yaml`](../modules/ms-teams.yaml) |
| Google Calendar | http + OAuth2 | New or Changed Event (trigger), List Events, Get / Create / Update / Delete Event, List Calendars | [`modules/google-calendar.yaml`](../modules/google-calendar.yaml) |
| Google Drive | http + OAuth2 | List Files, Get File, Create Folder, Copy File, Rename or Move File, Move to Trash, Delete Permanently, Share File, Export Doc as Text | [`modules/google-drive.yaml`](../modules/google-drive.yaml) |
| HubSpot | http + OAuth2 | Get / Search / Create / Update Contact, Get / Search / Create / Update Deal, Create Company, List Owners | [`modules/hubspot.yaml`](../modules/hubspot.yaml) |
| Salesforce | http + OAuth2 | SOQL Query, Get / Create / Update / Upsert / Delete Record of any sObject | [`modules/salesforce.yaml`](../modules/salesforce.yaml) |
| SharePoint | http + OAuth2 | Search Sites, Get Site, List Lists, List / Get / Create / Update / Delete Items, List Document Libraries, List Files and Folders | [`modules/sharepoint.yaml`](../modules/sharepoint.yaml) |
| Email (SMTP/IMAP) | go + credential | Email Received (trigger), Send Email, Read Emails, Move Email, Set Email Flags ([docs](email.md)) | [`modules/email`](../modules/email) |
| Telegram | wasm (Go and JS) | Send Message, Get Bot Info | [`examples/plugins/telegram`](../examples/plugins/telegram) |
| Mistral AI | wasm (Go) + credential | Chat (prompt → answer) | [`examples/plugins/mistral`](../examples/plugins/mistral) |

The `examples/plugins` are built and imported separately (`make plugin-sample`, `make plugin-mistral`).

## Installing

- **At startup:** put them into `G2F_MODULES_DIR`: `*.yaml`, `*.zip` bundles, or folders with
  `plugin.yaml` (+ `.wasm`). These are read-only in the UI and listed as *bundled*: the operator
  put them on disk, so they are trusted without a signature.
- **At runtime:** import them on the *Plugins* page or with `POST /api/plugins` (multipart
  field `file`, optional `signature`), or install them from a [catalog](#plugin-catalog), as a
  user with the `g2f-plugin-admin` realm role. Imported plugins are stored in PostgreSQL and
  loaded by all replicas. `DELETE /api/plugins/{module}` uninstalls one.

Plugins are installation-wide, which is why importing them needs a dedicated role separate
from organization admins.

**Replacing is explicit.** Importing a module that is already imported fails with `409` unless
the request sets `replace=true`. The response says what would be replaced:

```json
{ "error": "plugin \"github\" is already installed (version 1.0.0); confirm to replace it",
  "conflict": { "current":  { "module": "github", "name": "GitHub", "version": "1.0.0", "signer": "acme" },
                "incoming": { "module": "github", "name": "GitHub", "version": "1.1.0" } } }
```

The UI asks *Replace GitHub 1.0.0 (signed by acme) with 1.1.0 (unsigned)?*. This matters
because every organization's settings, tokens and credentials of that module are handed to the
new code. An import is saved to the database first and only then loaded, so a failed import
changes nothing. Modules from `G2F_MODULES_DIR` cannot be replaced by imports.

## Signing plugins

An operator can require that imported plugins are signed by a publisher they trust, and the
*Plugins* page shows for every plugin who signed it (*signed by acme*), *unsigned*, *bundled*
(modules directory) or *blocked* with the reason.

### Workflow

```bash
# Once, by the publisher: a key pair. acme.key is the private key (mode 0600, keep it secret);
# the printed line goes into the operator's G2F_PLUGIN_TRUSTED_KEYS.
g2f plugin keygen --name acme
# acme=x0hX1Mb1z3…=

# For every release: writes telegram.zip.sig next to the bundle.
g2f plugin sign --key acme.key telegram.zip

# Check it like the server will (--keys defaults to $G2F_PLUGIN_TRUSTED_KEYS).
g2f plugin verify --keys 'acme=x0hX1Mb1z3…=' telegram.zip
```

`g2f` is the server binary (`go run ./cmd/g2f plugin …` in a checkout; in the container image,
`docker run --rm -v "$PWD:/w" -w /w g2f:latest plugin …`). Upload the bundle together with its
`.sig` file in the import dialog, or pass the signature as the multipart field `signature`.

The operator configures (see [Configuration](configuration.md)):

| Variable | Meaning |
| --- | --- |
| `G2F_PLUGIN_TRUSTED_KEYS` | `acme=<base64 public key>,other=<…>`: the accepted publishers |
| `G2F_PLUGIN_REQUIRE_SIGNED` | `true`: only plugins with a valid signature by one of these keys |

| Upload | `REQUIRE_SIGNED=false` (default) | `REQUIRE_SIGNED=true` |
| --- | --- | --- |
| no signature | imported, shown as *unsigned* | refused |
| valid signature by a trusted key | imported, *signed by &lt;key&gt;* | imported |
| signature by a key that is not trusted | refused | refused |
| signature that does not match | refused | refused |

A plugin that carries a signature that does not verify is always refused, never imported as
unsigned: it means the bundle or the signature was changed, or the key is not trusted.

### Format

The signature covers the uploaded file byte for byte, a `.yaml` manifest or a `.zip` bundle
alike. It is Ed25519 over this message (UTF-8, `\n` line ends, lowercase hex):

```
g2f-plugin-v1
kind:<zip|yaml>
sha256:<sha256 of the file>
```

with a final newline after the hash. `kind` comes from the file extension (`.zip`; `.yaml` or
`.yml`), so a signature cannot be reused for the same bytes read as the other format, and the
`g2f-plugin-v1` prefix keeps plugin signatures apart from anything else a key may sign. The
detached signature file (`<bundle>.sig`) is a JSON envelope:

```json
{ "keyId": "acme", "alg": "ed25519", "signature": "<base64 of the 64-byte signature>" }
```

Unknown fields, other algorithms and signatures of the wrong size are rejected. Because the
exact bytes are signed, any change to a zip (an added, removed, renamed or reordered entry, or
different compression) needs a new signature. Independently of signatures, zips with
duplicate entry names, more than one `plugin.yaml`/`manifest.yaml` or `.wasm` file of the same
name, or more than 256 entries are refused, so it is never ambiguous which file is used.

The private key file written by `keygen` is JSON with the key name and the base64 32-byte
Ed25519 seed. Any Ed25519 implementation can produce compatible signatures.

### Stored plugins and replicas

The bundle is stored as uploaded, with its signature and signer. Every replica verifies the
stored bundle again (digest, signature, current policy) before it loads it, on startup and on
every sync. A plugin that fails is **blocked**: its nodes are not registered, the log says
`plugin blocked, not loading it` with the reason, and the *Plugins* page shows it with the
reason; the server keeps running. This happens when:

- `G2F_PLUGIN_REQUIRE_SIGNED` is turned on while unsigned plugins are imported: they stay in the
  database but stop loading after the restart. Re-import them signed (with replace) or uninstall
  them. Turning the setting off loads them again.
- a key is removed from `G2F_PLUGIN_TRUSTED_KEYS`: plugins signed with it are blocked (with
  either setting), which is how a key is revoked.
- a database row was changed: the bundle no longer matches its digest or signature.

Workflows that use the nodes of a blocked plugin fail at those nodes until it loads again.

### What a signature guarantees

A valid signature means: this exact file was signed with the private key the operator
trusts under that name. It authenticates the publisher and protects against changes in
transit, in a catalog and in the database. It does **not** mean the plugin is safe or was
reviewed: a signed plugin gets exactly the sandbox of an unsigned one (WASM limits,
`allowedHosts`, the network rules) and the settings and credentials its organizations
configure. Trust a key only as far as you trust its owner and how they keep the key. Anyone
with write access to the database can still delete plugins, and without
`G2F_PLUGIN_REQUIRE_SIGNED` they can store unsigned ones.

## Plugin catalog

Instead of uploading files, a plugin admin can install and update plugins from catalogs on the
*Catalog* tab of the *Plugins* page. Set `G2F_PLUGIN_CATALOGS` to one or more index URLs; without
it the tab is hidden.

A catalog is a JSON index served over https:

```json
{
  "name": "ACME plugins",
  "plugins": [
    {
      "module": "telegram",
      "name": "Telegram",
      "version": "1.2.0",
      "description": "Send messages with a Telegram bot",
      "runtime": "wasm",
      "url": "telegram-1.2.0.zip",
      "sha256": "9f2c…64 hex characters…",
      "signature": { "keyId": "acme", "alg": "ed25519", "signature": "q83v…" },
      "homepage": "https://acme.example/plugins/telegram",
      "minG2F": "1.4.0"
    }
  ]
}
```

| Field | |
| --- | --- |
| `module`, `version`, `url`, `sha256` | required. `url` may be relative to the index URL; it must end in `.zip`, `.yaml` or `.yml` |
| `signature` | the [signature envelope](#format) inline, or `signatureUrl` pointing to the `.sig` file |
| `name`, `description`, `runtime`, `homepage` | shown in the list |
| `minG2F` | oldest glue2flow version the plugin works with; not checked on development builds |

Entries with errors are listed with the reason and cannot be installed. The list shows per
entry whether it is installed, the same version, an update, or blocked, and the signature
state (*trusted key*, *unknown key*, *unsigned*). Installing downloads the bundle, checks it
against `sha256`, and then imports it exactly like an upload: the same signature rules apply,
replacing an installed version needs confirmation, and nothing is loaded before it has been
verified. The bundle must contain the module the entry names.

Fetching:

- The index URLs come from the operator and are fetched without the [SSRF guard](nodes.md#network-access).
  They must be https (http only on `localhost`).
- Bundle and signature URLs come from the index, i.e. from the catalog's publisher. They must be
  https and are fetched through the SSRF guard, except when they are on the index's own origin
  (same scheme and host). Redirects to other hosts are refused.
- The index is limited to 1 MiB and 1000 entries, bundles to the import limit (50 MiB);
  downloads time out after 2 minutes. Indexes are cached for 5 minutes (failures for 1 minute);
  *Refresh* fetches them again.
- An unreachable catalog is shown as unavailable with the reason; the rest of the page works.

API (plugin admins): `GET /api/plugins/catalog` and `POST /api/plugins/catalog/refresh` return
`{"requireSigned": bool, "catalogs": [{"url", "name", "error", "fetchedAt", "plugins": [{"module",
"name", "version", "description", "keyId", "verification", "state", "installedVersion",
"installable", "reason", …}]}]}`; `POST /api/plugins/catalog/install` with
`{"catalog": "<index url>", "module": "telegram", "replace": false}` installs (`409` as for
uploads when it would replace).

### Publishing a catalog

Put the bundles into a directory and let the server binary write the index:

```bash
g2f plugin catalog ./dist --key acme.key --name "ACME plugins"
# ./dist/index.json: 2 plugins
```

It reads every `.zip`/`.yaml` in the directory, takes module, name, version and description
from the manifests, and adds the sha256 and, with `--key`, a signature (without `--key`, an
existing `<bundle>.sig` is included). URLs are relative to the index unless `--base-url` is
given. Serve the directory over https and add the index URL to `G2F_PLUGIN_CATALOGS`. An index
can also be written by hand: `sha256sum` gives the digest, `g2f plugin sign` the signature.

## WebAssembly plugins

WASM plugins run in a fresh instance per node execution with no file system access and a
memory and time limit. They can make HTTP calls only to the manifest's `allowedHosts`, and
every redirect hop must stay within them. Plugin requests are also subject to the
[network access](nodes.md#network-access) rules: hosts in `allowedHosts` that resolve to
private addresses are blocked unless the administrator allows them.

See **[examples/plugins/telegram](../examples/plugins/telegram)** for a complete WASM plugin in
Go and JavaScript, the JSON calling convention and build instructions, and
**[examples/plugins/mistral](../examples/plugins/mistral)** for a compact Go plugin that calls an LLM
with a user's API key from a [credential](connections.md#api-key-credentials).

The input of `execute` carries, besides `items` (with resolved `params`):

| Key | Content |
| --- | --- |
| `config` | the organization's plugin-only settings |
| `vars` | the organization's variables |
| `oauth` | `{accessToken, tokenType, instanceUrl}` of the node's connected account (`oauth2` plugins) |
| `credential` | the field values of the node's credential, decrypted, defaults applied (`credentials` plugins; absent when an optional credential is not selected) |
| `workflow`, `execution` | ids and name |

For WASM plugins, `credentials.apiHosts` defaults to `allowedHosts`, which bound where the
plugin can send anything.

## Native Go modules

WASM plugins can only make HTTP calls. For other protocols a module is written in Go and
compiled into glue2flow, e.g. [`modules/email`](../modules/email):

- `modules/<name>/plugin.yaml` is a normal manifest with `runtime: go`. It declares the
  nodes, parameters, `config`, `vars`, `credentials` and output schemas, so settings,
  secrets, credentials (`ExecContext.Credential`) and the editor work exactly as for other plugins.
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
timeout: 20s                  # wasm: per node run (default 30s, at most 5m)
memoryMB: 32                  # wasm: default 64, at most 1024

config:                       # plugin-only settings per organization (admin, encrypted)
  - { name: botToken, label: Default bot token, type: secret }
  - { name: apiBaseUrl, default: https://api.telegram.org }
vars:                         # per-organization values usable in node fields as {{ vars.x }}
  - { name: defaultChatId, label: Default chat ID }
params:                       # parameters shared by all nodes of the module
  - { name: botToken, label: Bot token, type: secret }   # per-node secret
# credentials:                # or: named, shareable credentials users pick in nodes
#   fields: [{ name: apiKey, label: API key, type: secret, required: true }]
#   optional: true            # apiHosts: see docs/http-modules.md#credentials
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
OAuth2 (`oauth2:`) and credentials (`credentials:`) in [Connections](connections.md) and
[HTTP modules](http-modules.md#credentials). A module uses either `oauth2` or `credentials`.

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

#### Push

A polling trigger can declare how its source wakes it up (`push`), so the check runs right
away instead of at the next interval ([push delivery](triggers.md#push-delivery-for-mail-triggers)).
glue2flow adds the *Delivery* parameter (automatic / polling only).

- `endpoint: listen` (Go modules): the module keeps a connection open, see
  `nodes.RegisterListener` and the IMAP IDLE listener in [`modules/email`](../modules/email/idle.go).
- `endpoint: gmail` or `endpoint: graph` (http modules with `oauth2`): glue2flow subscribes at
  the provider with the requests below and receives the notifications on its own endpoint of
  that kind. The two endpoint kinds (Pub/Sub push of Gmail watches, Microsoft Graph change
  notifications) are implemented in Go because authenticating and decoding notifications is
  protocol code, not a request template; the subscription calls are templates.

```yaml
    push:
      endpoint: graph
      per: trigger            # or connection: one subscription per account, shared by its triggers
      requires: []            # config entries that must be set, e.g. [pubsubTopic]
      lifetime: 4200m         # push.expires = now + lifetime
      renewEvery: 24h         # default 24h; always at least 1h before expiry
      subscribe:   { method: POST, url: "https://graph.microsoft.com/v1.0/subscriptions", body: { … } }
      renew:       { method: PATCH, url: "https://graph.microsoft.com/v1.0/subscriptions/{{ push.id }}", body: { … } }  # default: subscribe again
      unsubscribe: { method: DELETE, url: "https://graph.microsoft.com/v1.0/subscriptions/{{ push.id }}" }
      id: "{{ response.body.id }}"
      expires: "{{ response.body.expirationDateTime }}"   # RFC 3339 or epoch milliseconds
```

The request templates see `params`, `config` and `oauth` like a node request, plus `push`:
`notificationUrl` (glue2flow's public endpoint), `clientState` (a random secret per
subscription, stored encrypted and checked on every notification), `expires` and `id`. Like
node requests they carry the account's token only to `oauth2.apiHosts` and go through the
SSRF guard. See [`gmail.yaml`](../modules/gmail.yaml) and [`outlook.yaml`](../modules/outlook.yaml).

### Parameters

Types: `string`, `text`, `number`, `boolean`, `select` (with `options`), `json`, `secret`,
`datetime`, `code`. Credential fields have their own types (`string`, `url`, `secret`,
`secretUrl`, see [Credentials](http-modules.md#credentials)). `showIf: {auth: [token, hmac]}` shows a field only for certain values of
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

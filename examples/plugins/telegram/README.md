# Telegram – sample WebAssembly plugin

A glue2flow plugin with two nodes, **Send Message** and **Get Bot Info**, implemented twice:
in [Go](go/main.go) and in [JavaScript](js/index.js). Both compile to the same kind of
WebAssembly module and work with the same [`plugin.yaml`](plugin.yaml).

```
telegram/
├── plugin.yaml     manifest: module, nodes, parameters, output schema, config, vars, sandbox settings
├── go/main.go      Go implementation (Extism Go PDK)
├── js/index.js     JavaScript implementation (Extism JS PDK)
├── js/index.d.ts   declares the exported function for the JS compiler
└── Makefile        build + bundle
```

## How a WASM plugin works

glue2flow runs plugins with [Extism](https://extism.org) on [wazero](https://wazero.io), a
pure-Go WebAssembly runtime (no CGO, no external runtime). Each node run gets a fresh,
sandboxed instance:

- no file system or environment access
- outbound HTTP only to the hosts listed in `allowedHosts` (and only via the PDK's HTTP API)
- memory limit (`memoryMB`) and time limit (`timeout`)

The plugin exports one function, `execute`. glue2flow calls it with JSON on the input:

```json
{
  "node": "sendMessage",
  "items": [
    { "json": { "chat": "1234" }, "params": { "chatId": "1234", "text": "Hi", "botToken": "…" }, "input": "main" }
  ],
  "config": { "botToken": "…", "apiBaseUrl": "https://api.telegram.org" },
  "vars": { "defaultChatId": "-1001234567890" },
  "workflow": { "id": "…", "name": "Notify" },
  "execution": { "id": "…" }
}
```

- `items[].json`: the incoming data item.
- `items[].params`: the node's parameters for that item, with `{{ expressions }}` already
  evaluated and secrets decrypted.
- `config`: the plugin-only settings of the workflow's organization (tokens, base URL).
- `vars`: the organization variables (also usable by editors in node fields).

The plugin writes `{"items": [...]}` (or `{"outputs": {"main": [...], "other": [...]}}`
for several outputs) to the output. Errors go through the PDK's error function
(`pdk.SetError` / `throw`) or `{"error": "message"}`. glue2flow strips URL paths from plugin
errors and scrubs known secret values from all error messages before storing them.

## Output schema

Each node declares the fields it emits under `outputSchema`, e.g. `message_id`, `chat.id`
and `text` for *Send Message*. The editor's ⚡ picker offers them in later nodes
(`{{ nodes['Telegram'][0].message_id }}`) before the workflow has ever run.

## Secrets and configuration

The manifest declares three kinds of settings:

| Kind | Declared in | Set by | Who can read it | Here |
| --- | --- | --- | --- | --- |
| **Config** | `config:` | organization admin, *Plugins* → *Settings* | **only the plugin code** | `botToken` (default bot), `apiBaseUrl` |
| **Vars** | `vars:` | organization admin, *Plugins* → *Settings* | editors, in node fields as `{{ vars.x }}` | `defaultChatId` |
| **Node secret** (`type: secret` param) | `params:` | workflow editor, in the node panel | only the plugin code | `botToken` (a user's own bot) |

Config and node secrets never appear in the expression scope of node fields. Editors
can't write `{{ config.botToken }}` into the message text: the workflow is rejected on
save. The plugin puts the token into the request URL itself, where Telegram expects it.
The base URL is config as well, since it only matters for building that URL.

Vars are for values editors should be able to use. The *Chat ID* field defaults to
`{{ vars.defaultChatId }}`, so an organization can set one alert chat for all nodes.

Secrets are encrypted at rest (AES-256-GCM, key `G2F_SECRET_KEY`) and write-only: the API
never returns them. The editor shows "stored encrypted" with *Replace* / *Clear*. The plugin
receives the decrypted values only while it runs. This sample uses the node's token when it
is set and falls back to the organization's default bot.

## Build

### Go (standard toolchain, Go ≥ 1.24)

```bash
make go          # GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm ./go
```

`//go:wasmexport execute` exports the function and `-buildmode=c-shared` builds a WASI
"reactor" module. Output is about 3 MB.

### Go with TinyGo (smaller, ≈ 0.5 MB)

```bash
make tinygo      # tinygo build -target wasip1 -buildmode=c-shared -o plugin.wasm ./go
```

### JavaScript

Install the [Extism JS PDK compiler](https://github.com/extism/js-pdk#install-the-compiler)
(`extism-js`). It needs [binaryen](https://github.com/WebAssembly/binaryen) (`wasm-merge`,
`wasm-opt`) on your `PATH`:

```bash
curl -O https://raw.githubusercontent.com/extism/js-pdk/main/install.sh && bash install.sh
make js          # extism-js js/index.js -i js/index.d.ts -o plugin.wasm
```

The compiler embeds a QuickJS engine plus your code into the module. The PDK provides
`Host.inputString()`, `Host.outputString()`, `Http.request()` and `Config.get()`.
`index.d.ts` must declare every exported function. For TypeScript or npm dependencies,
bundle to a single CommonJS file first (e.g. with esbuild) and compile the bundle.

### Package

```bash
make bundle      # -> telegram.zip containing plugin.yaml and plugin.wasm
```

## Install

As a user with the `g2f-plugin-admin` role (`alice` in the demo realm), choose one:

- **UI:** *Plugins* → *Import plugin* → choose `telegram.zip`.
- **API:**
  ```bash
  curl -X POST http://localhost:8080/api/plugins \
    -H "Authorization: Bearer $TOKEN" -F file=@telegram.zip
  ```
- **Directory:** copy the folder (with a built `plugin.wasm`) or the zip into
  `G2F_MODULES_DIR`. Plugins installed this way cannot be changed through the UI.

Imported plugins are stored in PostgreSQL and loaded by every glue2flow replica
(`G2F_PLUGIN_SYNC`). Importing the same module again replaces it (upgrade).

Then, as an organization admin, open *Plugins* → *Telegram* → *Settings* and set a
default bot token (and optionally a default chat ID). Or give each *Send Message* node its own token.

## Test a plugin without glue2flow

The [Extism CLI](https://github.com/extism/cli) can call the function directly:

```bash
extism call plugin.wasm execute --wasi --allow-host api.telegram.org --input '{
  "node": "getMe", "config": {}, "vars": {}, "items": [{"json": {}, "params": {"botToken": "123:ABC"}}]
}'
```

glue2flow's own test suite builds the Go version of this plugin and runs it against a fake
Telegram API (`internal/plugin/plugin_test.go`).

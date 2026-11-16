[Docs](README.md) › Secrets & plugin settings

# Secrets & plugin settings

Secret values never stay in the workflow JSON.

| Where | Set by | Scope | Readable by | Use for |
| --- | --- | --- | --- | --- |
| [Per-node secrets](#per-node-secrets) | workflow editor, in the node | one node | that node's plugin | a value that really belongs to one node (e.g. a signing secret of one webhook) |
| [Credentials](connections.md#api-key-credentials) | any user, *Connections* | owner, shared with groups or the organization | the plugin of every node that selects it | a user's or bot's API key, token or webhook URL |
| [Connected accounts](connections.md) | any user, OAuth2 sign-in | as credentials | as credentials | OAuth2 providers |
| [Plugin config](#plugin-settings-config-and-vars) | organization admin, *Plugins* | the whole organization | the plugin, for every node | app credentials, organization-wide defaults |

## Per-node secrets

Node parameters of type `secret`. On save they are moved into `node_secrets`. The API returns `{"$secret": "stored"}` instead of the value;
sending that placeholder back keeps the value, and an empty value deletes it. The engine
decrypts them right before the node runs.

## Plugin settings: config and vars

Declared in the plugin's manifest and set once per organization by an organization admin on
the *Plugins* page (`GET/PUT /api/plugins/{module}/config`). There are two kinds:

| Kind | Manifest section | Readable by | Typical content |
| --- | --- | --- | --- |
| **config** | `config:` | the plugin only: WASM code (`config`) and the manifest's request templates (`{{ config.x }}`) | API tokens, client secrets, base URLs |
| **vars** | `vars:` | workflow editors, in node fields: `{{ vars.x }}` | default channel or chat, team name |

Config never reaches expressions in node fields. A field such as a message text cannot
contain the bot token: saving a workflow whose node fields reference `config` fails, and at
run time `config` does not exist in their scope. The plugin decides where config values go
(URL, `Authorization` header, `X-…` headers, request body). Vars cannot be secrets.
Secret config entries are write-only.

## Encryption

All of these are encrypted with AES-256-GCM using `G2F_SECRET_KEY`. Each ciphertext is bound to its
location (workflow/node/parameter, org/module/key, or credential/field), so it cannot be
copied into another row. Error messages are scrubbed of known secret values before they are stored.

Key rotation: set a new `G2F_SECRET_KEY` and put the old one into `G2F_SECRET_KEYS_OLD`.
Old values stay readable; values are re-encrypted with the new key when they are saved again.

```bash
openssl rand -base64 32   # generate a key
```

OAuth2 tokens of connected accounts and secret credential fields are encrypted the same way,
see [Connections](connections.md).

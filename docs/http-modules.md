[Docs](README.md) › HTTP modules

# HTTP modules

Most integrations are thin wrappers around a REST API. They can be written as YAML and
dropped into `modules/` (loaded at startup) or imported on the *Plugins* page. See
[`modules/github.yaml`](../modules/github.yaml):

```yaml
module: github
name: GitHub
baseUrl: https://api.github.com
credentials:                  # users store their token once, see "Credentials" below
  optional: true
  fields:
    - { name: token, label: Personal access token, type: secret, required: true }
headers:
  Authorization: "{{ (credential.token ?? '') != '' ? 'Bearer ' + credential.token : '' }}"   # empty headers are dropped
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
    outputSchema:                   # fields offered by the ⚡ picker
      fields:
        - { path: full_name, type: string }
        - { path: stargazers_count, type: number }
```

Inside the request templates (`url`, `path`, `headers`, `query`, `body`, `output`), `params`
holds the node's resolved parameters (secrets decrypted), `config` the module's
plugin-only settings, `credential` the fields of the node's [credential](#credentials) and
`oauth` the [connected account](connections.md)'s token. For example:
`Authorization: "Bearer {{ config.apiToken }}"`. Node fields only see `vars`.

`output` is an expression over `response` (`status`, `headers`, `body`) that produces the
emitted item(s); it defaults to `{{ response.body }}`. The node fails on HTTP status ≥ 400
unless `failOnError: false` is set.

Requests use the same [network access](nodes.md#network-access) rules as the HTTP Request
node. Credentials of a module (headers, query, OAuth token) are never forwarded to another
host: a redirect that changes scheme, host or port fails with
`redirect from … refused: module credentials are only sent to the configured API host`;
redirects within the same host are followed. The OAuth token is sent only over `https` to the
default port of a host in `apiHosts`; an entry can name another port (`api.example.com:8443`).

## Path values are escaped

Every `{{ }}` block in a `path` (and in the path part of a `url`, between the host and a
`?` or `#`) is rendered and percent-encoded as **one path segment**: a value like `a/b`
becomes `a%2Fb`, `x?y=1#z` becomes `x%3Fy=1%23z`, so a parameter cannot reach another endpoint
of the API host with the stored credential. A value that is exactly `.` or `..` fails the
node. The text you write around the blocks, including its slashes, is used as written, and
so is `baseUrl`. `query` values are encoded by the request as well. In a `url` the host part and the query part are
not encoded; use `urlEncode(x)` for a value inside a hand-written query string. A `url` that
is a single value (Slack's `{{ credential.webhookUrl }}`) is not a path template and stays as is.

Some parameters legitimately hold slashes or arrive pre-encoded, e.g. SharePoint site IDs
(`contoso.sharepoint.com:/sites/team`, `host,guid,guid`) or Graph paths (`root:/folder/file.txt:`).
Wrap them in `rawPath(x)`:

```yaml
path: /sites/{{ rawPath(params.siteId) }}/lists
```

`rawPath` inserts the value unescaped but still fails the node for a `.` or `..` segment
(also percent-encoded), `?`, `#`, backslashes and control characters. It marks the whole
block value: to build a value from several parts, escape the user-supplied parts yourself with
`pathEscape(x)`: `{{ rawPath('drives/' + pathEscape(params.driveId)) }}`. Use `rawPath` only for
parameters documented to take slashes.

## Per-account API host

Some providers give every account its own API host (Salesforce: `https://acme.my.salesforce.com`).
Name the token response field that holds it with `oauth2.instanceUrlField`:

```yaml
baseUrl: "{{ oauth.instanceUrl }}"
oauth2:
  instanceUrlField: instance_url
  apiHosts: ["*.my.salesforce.com"]
```

When an account is connected, the field must be present and an `https` origin (no credentials,
path, query or fragment) whose host is allowed by `apiHosts`; otherwise connecting fails. It is
stored with the connection (not secret), kept if a refresh response leaves it out, and shown on the *Connections*
page. `{{ oauth.instanceUrl }}` is available in request templates only, like `oauth.accessToken`;
node fields cannot read it. WASM plugins get it as `oauth.instanceUrl`.

`apiHosts` entries are hosts (optionally with a port) or wildcards `*.example.com`, which match
any subdomain (`a.example.com`, `a.b.example.com`) but not `example.com` itself or
`evilexample.com`. A wildcard needs at least two labels after `*.` (`*.com` is rejected when the
manifest loads). The https and port rules above apply to wildcard entries too.

## Credentials

For APIs without OAuth2, a module declares the fields of a credential. Users create named
credentials on the *Connections* page, share them like accounts and pick one in the
**Credential** field that every node of the module gets (see
[API-key credentials](connections.md#api-key-credentials)):

```yaml
baseUrl: "{{ credential.apiUrl }}"
credentials:
  fields:
    - { name: token, label: Personal access token, type: secret, required: true }
    - { name: apiUrl, label: API URL, type: url, default: https://api.github.com }
  apiHosts: [api.github.com, "*.ghe.com"]   # default: host of a fixed baseUrl
  optional: true                            # nodes may run without a credential
headers:
  Authorization: "Bearer {{ credential.token }}"
```

| Field type | Stored | Shown by the API | Checked |
| --- | --- | --- | --- |
| `string` | readable | yes | – |
| `url` | readable | yes | https URL on an `apiHosts` host |
| `secret` | encrypted | only whether it is set | – |
| `secretUrl` | encrypted | only whether it is set | https URL on an `apiHosts` host |

Fields also take `label`, `description`, `placeholder`, `required` and (non-secret only)
`default`. `{{ credential.<field> }}` is available in request templates only; without a
selected credential (`optional: true`) it holds the field defaults. Node fields cannot read it.

Every request of a module with `credentials` must go to an `apiHosts` host (same rules as
`oauth2.apiHosts`: exact hosts or `*.example.com` wildcards, https, port 443 unless an entry
names one). Anything else fails before the request is sent, whether the URL comes from a node
parameter or a credential field, and redirects to another host are refused. A `url` /
`secretUrl` field is checked against `apiHosts` when the credential is saved and again for
every request, so a URL-shaped secret can be used directly:

```yaml
credentials:                  # modules/slack.yaml: the webhook URL is the secret
  fields:
    - { name: webhookUrl, label: Webhook URL, type: secretUrl, required: true }
  apiHosts: [hooks.slack.com]
nodes:
  - type: webhook.post
    request: { method: POST, url: "{{ credential.webhookUrl }}", body: { text: "{{ params.text }}" } }
```

`apiHosts` always comes from the manifest: a `url` field lets a credential choose among the
allowed hosts (e.g. GitHub Enterprise Cloud hosts under `*.ghe.com`), never add one. Hosts a
user picks freely (self-hosted servers) are not supported; install a copy of the module with
that host instead. A module uses either `oauth2` or `credentials`.

The full list of manifest keys, parameter types and `outputSchema` options is in the
[manifest reference](plugins.md#manifest-reference). For logic that does not fit a single HTTP
request, write a [WebAssembly plugin](plugins.md#webassembly-plugins) or a
[built-in node in Go](nodes.md#writing-nodes-in-go).

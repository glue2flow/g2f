[Docs](README.md) › HTTP modules

# HTTP modules

Most integrations are thin wrappers around a REST API. They can be written as YAML and
dropped into `modules/` (loaded at startup) or imported on the *Plugins* page. See
[`modules/github.yaml`](../modules/github.yaml):

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
    outputSchema:                   # fields offered by the ⚡ picker
      fields:
        - { path: full_name, type: string }
        - { path: stargazers_count, type: number }
```

Inside the request templates (`url`, `path`, `headers`, `query`, `body`, `output`), `params`
holds the node's resolved parameters (secrets decrypted) and `config` the module's
plugin-only settings. For example:
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

The full list of manifest keys, parameter types and `outputSchema` options is in the
[manifest reference](plugins.md#manifest-reference). For logic that does not fit a single HTTP
request, write a [WebAssembly plugin](plugins.md#webassembly-plugins) or a
[built-in node in Go](nodes.md#writing-nodes-in-go).

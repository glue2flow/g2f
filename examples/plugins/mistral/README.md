# Mistral AI – WebAssembly plugin (Go)

A glue2flow plugin with one node, **Chat**: it takes the incoming item, sends a prompt to
[Mistral AI](https://docs.mistral.ai/api/) (`/v1/chat/completions`) and outputs the answer
(input → output). Modeled after the [Telegram sample](../telegram); see its README for how WASM
plugins, config, vars and secrets work.

```
mistral/
├── plugin.yaml   manifest
├── go/main.go    Go implementation (Extism Go PDK)
└── Makefile      build + bundle
```

## Node: Chat

Runs once per input item.

| Parameter | Description |
| --- | --- |
| Model | e.g. `mistral-small-latest`; defaults to `{{ vars.defaultModel }}` |
| System prompt | optional instructions |
| Prompt | defaults to `{{ json.text }}`, may use any expression |
| Temperature | optional, 0–1.5 |
| Max tokens | optional |
| Force JSON output | sets `response_format: json_object`; the parsed object is added as `data` |

Output item:

```json
{
  "text": "the answer",
  "model": "mistral-small-latest",
  "finishReason": "stop",
  "usage": { "promptTokens": 12, "completionTokens": 40, "totalTokens": 52 },
  "data": { }
}
```

(`data` only with *Force JSON output*.)

## Keys

- **Config** `apiKey` (secret, Plugins → Settings): organization-wide default key.
- **Node secret** `apiKey`: a user's own key; overrides the default.
- **Config** `apiBaseUrl`: defaults to `https://api.mistral.ai`. If changed, add the host to `allowedHosts`.
- **Var** `defaultModel`: usable in node fields as `{{ vars.defaultModel }}`.

## Build and install

```bash
make go        # or: make tinygo
make bundle    # -> mistral.zip
```

Import `mistral.zip` via *Plugins* → *Import plugin*, then set the API key under *Settings*.
The test `internal/plugin/mistral_test.go` builds the plugin and runs it against a fake API.

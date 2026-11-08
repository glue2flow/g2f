[Docs](README.md) › Connected accounts (OAuth2)

# Connected accounts (OAuth2)

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

## Manifest

[`modules/google-sheets.yaml`](../modules/google-sheets.yaml) (Append Row, Read Rows) is a
complete example. It stays inactive until an admin enters the Google OAuth client ID and secret.

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

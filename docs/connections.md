[Docs](README.md) › Connections

# Connections

The *Connections* page holds two kinds of reusable sign-ins that nodes select instead of
carrying secrets themselves:

- **Connected accounts (OAuth2)**: users connect their own accounts at a provider, such as
  Google, Microsoft or HubSpot. The host runs the whole OAuth2 flow and stores the tokens, and
  plugins only receive a valid access token while a node runs.
- **[API-key credentials](#api-key-credentials)**: for services without OAuth2, a named set of
  values (a personal access token, a webhook URL, a username and password) that a user enters
  once and picks in any node of that plugin.

Both are shared the same way and selected in the node's **Account** / **Credential** field.

## Connected accounts (OAuth2)

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
| Flow security | Authorization code flow with PKCE (`pkce: true`). The `state` is an encrypted, 10-minute blob bound to the user, plugin and PKCE verifier, so no server-side session is needed. The flow is also bound to the browser that started it by a short-lived `HttpOnly` cookie (`g2f_oauth`) whose hash is stored in the state, so a provider link forwarded to someone else cannot be completed. |
| Token delivery | HTTP plugins: added as `Authorization` header (or `{{ oauth.accessToken }}` in manifest templates), **only for `apiHosts`** (exact hosts or `*.example.com` wildcards). Requests to any other host are refused. WASM plugins: `input.oauth.accessToken`. Node fields cannot read the token. |
| Sharing | A connection belongs to whoever connected it and is *Only me*, shared with *Groups*, or the *Organization*. Selecting an account in a node requires access to it. Co-editors can still save workflows using someone else's account if they do not change that selection. Only the owner or an org admin can rename, share or remove it. |
| Errors | Known token values are scrubbed from error messages. |

### Included OAuth2 plugins

Each stays inactive until an admin enters the client ID and secret; the setup steps are at the
top of each file.

| Module | Nodes | Provider app |
| --- | --- | --- |
| [`gmail`](../modules/gmail.yaml) | **New Email** (trigger), Send Email (HTML, attachments, replies in threads), Search Messages, Get Message, Get Attachment, Change Labels (mark read, archive, star), Move to Trash, List Labels | Google Cloud OAuth client, Gmail API |
| [`google-sheets`](../modules/google-sheets.yaml) | Append Row, Read Rows | Google Cloud OAuth client, Sheets API |
| [`outlook`](../modules/outlook.yaml) | **New Email** (trigger), Send Email, List Messages (unread, search, OData filter), Get Message, Reply / Reply all, Move Message, Mark as Read, List Folders | Microsoft Entra app (Graph `Mail.ReadWrite`, `Mail.Send`) |
| [`ms-teams`](../modules/ms-teams.yaml) | Send Channel Message, Reply in Channel Thread, Send Chat Message, List Teams, List Channels, List Chats | Microsoft Entra app (Graph, no admin consent needed) |
| [`google-calendar`](../modules/google-calendar.yaml) | **New or Changed Event** (trigger), List Events, Get / Create / Update / Delete Event (attendees, Google Meet), List Calendars | Google Cloud OAuth client, Calendar API |
| [`google-drive`](../modules/google-drive.yaml) | List Files (search), Get File, Create Folder, Copy File, Rename or Move File, Move to Trash, Delete Permanently, Share File, Export Doc as Text (metadata and folders only, no file upload or download) | Google Cloud OAuth client, Drive API (restricted scope `drive`) |
| [`hubspot`](../modules/hubspot.yaml) | Get / Search / Create / Update Contact, Get / Search / Create / Update Deal, Create Company, List Owners | HubSpot app (CRM scopes) |
| [`salesforce`](../modules/salesforce.yaml) | SOQL Query, Get / Create / Update / Upsert (external ID) / Delete Record of any sObject | Salesforce Connected App; the org's host comes from the token response, see below |
| [`sharepoint`](../modules/sharepoint.yaml) | Search Sites, Get Site, List Lists, List / Get / Create / Update / Delete Items, List Document Libraries, List Files and Folders | Microsoft Entra app (Graph `Sites.ReadWrite.All`, admin consent) |

Google modules can share one OAuth client, as can the Microsoft modules (Outlook, Teams,
SharePoint). The Microsoft manifests use the `common` endpoint (multi-tenant apps); for a
single-tenant app, put your tenant ID in `authorizeUrl` and `tokenUrl`.

#### Setup notes per provider

The redirect URI is always `<G2F_PUBLIC_URL>/oauth/callback`. The full steps are at the top
of each manifest.

- **Google Calendar / Drive:** in the Google Cloud console enable the *Google Calendar API* /
  *Google Drive API* and add the scopes on the consent screen: `calendar.events` and
  `calendar.calendarlist.readonly` (only for List Calendars) for Calendar, `drive` for Drive.
  `drive` is a *restricted* scope: Google requires an app verification unless the consent
  screen is *Internal* (your own Workspace organization). `drive.file` would not see existing
  files. The same OAuth client works for all Google modules.
- **HubSpot:** create an app in a HubSpot developer account, put the redirect URI on its
  *Auth* tab and enable the scopes `crm.objects.contacts.read/write`,
  `crm.objects.companies.read/write`, `crm.objects.deals.read/write` and
  `crm.objects.owners.read`. Enter the app's client ID and secret in the plugin settings.
- **Salesforce:** create a *Connected App* (Setup → App Manager) with the callback URL and the
  OAuth scopes `api`, `refresh_token`, `openid` and `email`, and enter its Consumer Key and
  Secret in the plugin settings. Nothing in the file needs editing: Salesforce returns the
  org's host (`instance_url`, e.g. `https://acme.my.salesforce.com`) with the token, glue2flow
  stores it with the connection and sends requests there. The access token only goes to
  `*.my.salesforce.com` (My Domain, including sandbox and scratch org hosts). **Sandboxes**
  sign in at `test.salesforce.com`, which the manifest cannot make a setting: install a copy of
  `modules/salesforce.yaml` with another module name (e.g. `salesforce-sandbox`) and
  `test.salesforce.com` in `authorizeUrl`, `tokenUrl` and `userInfo.url`.
- **SharePoint:** use a Microsoft Entra app as for Outlook and add the delegated Graph
  permissions `Sites.ReadWrite.All` and `offline_access`. `Sites.ReadWrite.All` requires
  **admin consent** (*Grant admin consent* on the API permissions page); without it users see
  an "approval required" screen. Only work and school accounts have SharePoint. Users see
  only the sites they can open in SharePoint. The optional plugin variable *Default site ID*
  (e.g. `contoso.sharepoint.com:/sites/team`) saves entering the site in every node.

```
Gmail: New Email ("label:support") ─► Gmail: Get Message
   ─► Teams: Send Channel Message "New ticket: {{ json.subject }}" ─► Gmail: Change Labels (remove UNREAD)
```

### Manifest

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
  apiHosts: [sheets.googleapis.com]   # default: host of baseUrl; also "*.example.com"
  # instanceUrlField: instance_url    # per-account API host, see docs/http-modules.md
  # tokenAuth: basic                  # client auth at the token endpoint (default: body)
  # clientIdConfig / clientSecretConfig: names of the config entries (default clientId / clientSecret)
```

## API-key credentials

A credential belongs to the user who created it (*Connections → 🔑 Add credential*, or *＋ Add*
next to a node's **Credential** field) and holds the fields the plugin declares, e.g. "GitHub –
deploy bot" with a personal access token. It is shared like an account (*Only me*, *Groups*,
*Organization*), and every node of that plugin can select it, so a token is no longer entered
in every node or shared with the whole organization through the plugin settings.

| Topic | Behaviour |
| --- | --- |
| Storage | A row of the connections table (`kind: credential`). Secret fields (`secret`, `secretUrl`) are encrypted (AES-256-GCM) and bound to the credential and field, so a ciphertext copied elsewhere does not decrypt. Other fields (`string`, `url`) are stored readable and shown on the *Connections* page. |
| API | `POST /api/connections` `{"module", "name", "fields"}` creates one, `PATCH /api/connections/{id}` changes `fields` (and, as for accounts, `name`, `scope`, `groups`), `DELETE` removes it. Secret values are never returned: the API lists `fields` (non-secret values) and `secrets` (which secret fields are set). On update an omitted field or `{"$secret": "stored"}` keeps the value and `""` clears it. Unknown fields, missing required fields and URLs on other hosts are rejected. |
| Delivery | Only to the plugin while the node runs: HTTP manifests as `{{ credential.<field> }}` in request templates, WASM plugins as `input.credential`, Go modules as `ExecContext.Credential`. Node fields cannot read them (saving a field that uses `credential.*` fails). |
| Hosts | Every request of an HTTP module with credentials must go to one of its `credentials.apiHosts` (https, same rules as `oauth2.apiHosts`), or the node fails before anything is sent; redirects to another host are refused. `url` and `secretUrl` fields must point to an allowed host, checked when the credential is saved and on every run. Hosts come from the manifest, which only a plugin admin installs: a credential can choose among them (e.g. a GitHub Enterprise Cloud host) but never add one. |
| Sharing | As for accounts: selecting a credential in a node requires access to it, co-editors can keep someone else's selection, only the owner or an org admin can edit, share or remove it. At run time the credential must belong to the workflow's organization and the node's plugin. |
| Errors | Secret values are scrubbed from error messages and execution logs. |

Plugins that use credentials:

| Module | Fields | Allowed hosts |
| --- | --- | --- |
| [`github`](../modules/github.yaml) | Personal access token; API URL (default `https://api.github.com`, or `https://api.<subdomain>.ghe.com`). Optional: without a credential, nodes read public data. | `api.github.com`, `*.ghe.com` |
| [`slack`](../modules/slack.yaml) | Incoming webhook URL (`secretUrl`: the URL is the secret) | `hooks.slack.com` |
| [`email`](../modules/email) | Username and password of a personal mailbox on the organization's mail server. Optional: without one, the organization's account is used. | the servers in the plugin settings |
| [`mistral`](../examples/plugins/mistral) (example) | API key. Optional: without one, the organization's default key is used. | `allowedHosts` |

GitHub Enterprise Server (self-hosted) is not covered by the bundled module, since its host
cannot be a user's choice: install a copy of `modules/github.yaml` under another module name
with your host in `baseUrl` / `credentials.apiHosts`.

The manifest syntax is in [HTTP modules](http-modules.md#credentials); where credentials sit
next to node secrets and plugin settings is in [Secrets & plugin settings](secrets.md).

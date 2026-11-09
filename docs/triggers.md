[Docs](README.md) › Triggers

# Triggers

Every workflow starts at a trigger node. A workflow may have several triggers.

| Trigger | Fires when | Item emitted |
| --- | --- | --- |
| **Manual** | a user with role `runner`+ clicks ▶ Run, or `POST /api/workflows/{id}/run` | the node's test payload (or `items` from the request body) |
| **Schedule** | the workflow is **active** and the time is due | `{firedAt, scheduledAt, timezone}` |
| **Webhook** | the workflow is **active** and `/webhook/{workflowId}/{nodeId}` is called | `{method, headers, query, body, receivedAt}` |
| **Form** | the workflow is **active** and someone submits the form at `/form/{workflowId}/{nodeId}` | the answers by question key, plus `submittedAt` (and `submittedBy` with login or access code) |
| **Called by Workflow** | an [Execute Workflow](nodes.md#execute-workflow) node of another workflow calls it | the caller's items |
| **Error Trigger** | a watched workflow fails | `{workflow, execution, failedNode, error}` |
| **Polling triggers** (Email Received, Gmail / Outlook New Email, …) | the workflow is **active** and a periodic check finds new items | one item per new item, e.g. per e-mail |

Any trigger can also be run manually from the editor (select it, then ▶ Run). Webhook and
schedule triggers then use their test payload, so a flow can be tested before activation.

## Polling triggers

Some sources cannot call a webhook, so glue2flow checks them every few minutes (*Check every
(minutes)*, default 5):

| Trigger | Plugin | Detects new items by |
| --- | --- | --- |
| **Email Received** | [Email (IMAP)](email.md) | the mailbox's message UIDs |
| **New Email** | [Gmail](connections.md#included-oauth2-plugins) (any Gmail search) | message IDs |
| **New Email** | [Outlook](connections.md#included-oauth2-plugins) (a folder, optionally unread or filtered) | message IDs |

- A run starts only when a check finds something new, so idle checks leave no executions.
- The first check after activation only remembers what is already there.
- ▶ Run in the editor shows the current items without remembering them.
- A failing check (e.g. a rejected login) is recorded as a failed execution, once while the
  same error repeats, so it shows up in the editor and notifies [error workflows](#error-trigger).
- Checks happen at times aligned to the interval (every full 5 minutes) and are claimed in
  PostgreSQL, so with several replicas each check runs once.

YAML plugins can declare their own polling triggers, see [Plugins](plugins.md#polling-triggers).

## Schedule

- *Recurring*: 5-field cron (`*/15 * * * *`), or descriptors like `@hourly`, `@daily`, `@every 30m`. At most once per minute.
- *Once*: a local date-time (`2026-12-24T18:00`).
- Each schedule has an IANA timezone (`Europe/Berlin`), so it follows daylight saving time.
- Each firing is claimed in PostgreSQL (`trigger_firings`) under a key derived from the scheduled time.
  With several replicas, every replica runs the scheduler and exactly one executes each firing.
  A one-time schedule never runs twice, even across restarts.
- Missed recurring runs (server down) are skipped. A one-time run missed by less than
  `G2F_SCHEDULE_CATCHUP` (default 1h) runs as soon as the server is back.

## Webhook

- Method: `POST` (default), `GET`, `PUT`, `PATCH`, `DELETE` or `ANY`.
- Authentication:
  - `none`
  - `token`: shared secret in a header (default `X-G2F-Token`, `Bearer ` prefix allowed)
  - `hmac`: HMAC-SHA256 signature of the raw body in a header (default `X-Signature-256`,
    value `sha256=<hex>`). Set the header to `X-Hub-Signature-256` for GitHub webhooks.
- The secret header and cookies are removed before the request becomes workflow data.
- Response:
  - `immediately`: `202 {"executionId": ...}`
  - `lastNode`: waits up to `G2F_WEBHOOK_MAX_WAIT` and returns the output of the last node
    (one item as an object, otherwise an array). Errors return HTTP 500.
- JSON and form bodies are parsed; anything else arrives as a string. Limit 5 MiB.
- The editor shows the URL and a ready-to-use `curl` command for the selected webhook node.

```bash
curl -X POST http://localhost:8080/webhook/<workflow-id>/<node-id> \
  -H 'X-G2F-Token: s3cret' -H 'Content-Type: application/json' -d '{"name":"Ada"}'
```

## Form

Publishes a simple web form, like a very small Microsoft Forms or LimeSurvey. Share the
*Form URL* shown in the editor; every submission starts one run.

- Question types: *Short text*, *Long text*, *Single choice* (radio buttons) and
  *Multiple choice* (checkboxes). Each question can be required and have a help text.
- Each answer becomes a field of the output item, named by the question's *key* (derived from
  the label, editable): text and single choice as a string (empty if unanswered), multiple
  choice as an array in the configured order. `submittedAt` is added.

  ```json
  {"name": "Ada", "food": "Sushi", "diet": ["Vegan"], "comment": "", "submittedAt": "2026-11-09T10:00:00Z"}
  ```
- Answers are checked on the server: required questions, choices must be one of the options,
  at most 1,000 characters for short and 10,000 for long text. Invalid submissions show the
  form again with the answers kept and do not start a run.
- After submitting, the page shows the *Message after submitting* (with a link to answer
  again). Reloading does not submit twice.
- Query parameters prefill answers: `…/form/<workflow-id>/<node-id>?team=Sales`.
- Deactivate the workflow to close the form. The page uses no JavaScript and works on phones.
- ▶ Run in the editor emits one sample answer per question (and a sample `submittedBy`).

### Protection

*Protection* decides who can answer:

| Protection | The person answering… | Adds to the item |
| --- | --- | --- |
| **None** | just opens the link | – |
| **Simple captcha** | also solves a small sum (`12 + 7 = ?`) | – |
| **Sign in with Keycloak** | signs in with their organization account | `submittedBy`: `sub`, `username`, `name`, `email` |
| **Access code by e-mail** | enters an address and the 6-digit code sent to it | `submittedBy.email` |

- **Captcha** keeps simple bots out, not determined attackers. Each question can be
  answered once; a wrong answer shows a new one, and the other answers are kept.
- **Sign in** uses the editor's Keycloak client (`G2F_OIDC_CLIENT_ID`, authorization code with
  PKCE). The form's URL must be allowed as a redirect URI of the client, e.g.
  `https://g2f.example.com/*` (the demo realm allows `http://localhost:8080/*`). Only members
  of the workflow's organization can answer. The backend redeems the code at the token
  endpoint, see `G2F_OIDC_TOKEN_URL` in [Configuration](configuration.md). With
  `G2F_AUTH_MODE=none` everyone answers as the development user.
- **Access codes** are sent with the organization's e-mail account (Plugins → Email →
  Settings, SMTP host and default sender). Without it the form shows *Form unavailable* and
  the editor warns. *Allowed e-mail domains* (e.g. `example.com`) restricts which addresses
  can request a code; subdomains are included. A code is valid for 15 minutes and once; after
  5 wrong tries a new one is needed. Each address can request 5 codes per hour, each client
  IP 20.
- After signing in or confirming the address, the browser remembers it for 12 hours for this
  form (an HTTP-only cookie). *Not you?* forgets a confirmed address.
- Tokens and cookies are signed with a key derived from `G2F_SECRET_KEY`; one-time use and
  limits are kept in PostgreSQL, so they work across replicas.

## Called by Workflow

Starts this workflow from an *Execute Workflow* node of another workflow. It receives the
caller's items; the output of the last node is returned to the caller. Its *Test items* are
used when the workflow is run from the editor.

## Error Trigger

Starts a workflow when a run fails. It can watch the workflow it belongs to
(an in-workflow error handler) or selected workflows (watching others requires viewer access
and an active error workflow). It emits `workflow` (with editor URL), `execution`,
`failedNode` and `error`. Manual test runs are ignored unless enabled, and failures of error
workflows never trigger further error workflows.

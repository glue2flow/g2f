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
- Checks happen at times aligned to the interval plus a fixed offset per trigger (derived from
  workflow and node id, the same on every replica), so triggers are spread over the interval
  instead of all checking at the same second. Each scheduled check is requested once in
  PostgreSQL, so with several replicas it runs once.
- Checks of one trigger never overlap, on any replica: whoever asks for a check (the clock or a
  [push notification](#push-delivery-for-mail-triggers)), a claim per trigger in PostgreSQL
  (`poll_checks`) lets one check run at a time. Requests that arrive during a check cause exactly
  one more check afterwards; a burst of requests becomes one or two checks. A request is only
  cleared by a check that started after it, so a replica that fails or stops before running
  the check leaves it to another one (picked up within seconds).
- Checks run on at most `G2F_POLL_WORKERS` (default 8) workers per replica.
- The mail triggers can be woken up by their mail server instead of waiting for the next check,
  see [Push delivery](#push-delivery-for-mail-triggers).

YAML plugins can declare their own polling triggers, see [Plugins](plugins.md#polling-triggers).

## Push delivery for mail triggers

The three mail triggers learn about new mail within seconds when their source can tell
glue2flow. A notification never carries the mail: it only runs the trigger's normal check
right away. The check stays the single source of truth (cursor, first-check baseline, "every
message once"), so a lost, duplicated or spurious notification costs at most one extra check.

| Trigger | Push source | Needs |
| --- | --- | --- |
| **Email Received** (IMAP) | IMAP IDLE: a connection that waits on the mailbox and reports new messages (`EXISTS`/`RECENT`) | a server with the `IDLE` capability (almost all) |
| Gmail **New Email** | Gmail watch → Google Cloud Pub/Sub topic → push subscription → `POST /push/gmail/<token>` | public https `G2F_PUBLIC_URL`, a Pub/Sub topic (setup below) |
| Outlook **New Email** | Microsoft Graph change notifications (subscription on the trigger's folder) → `POST /push/graph` | public https `G2F_PUBLIC_URL` |

The trigger's info box shows what is happening: *push · IMAP IDLE* (connected since …),
*push · Gmail* / *push · Microsoft Graph* (subscription renews …), or *polling every N min*
with the reason push is not active (turned off, `G2F_PUBLIC_URL` not public https, not set up,
server without IDLE, last error, …).

**Safety net.** While push works, the trigger is still checked at least every 15 minutes
(its *Check every* interval if that is longer), because notifications can get lost. When
push stops working (connection lost, subscription failed or expired), the configured interval
applies again automatically, at the latest 30 seconds later.

**Per trigger:** *Delivery* = *Automatic* (default) or *Polling only*. **Per server:**
`G2F_MAIL_PUSH=false` turns push off everywhere.

### IMAP IDLE

For each active Email Received trigger exactly one replica keeps an IDLE connection to the
mailbox (a lease in PostgreSQL, renewed every 10 s; if the replica stops, another one takes
over within about 40 s). The connection uses the same server and login as the check (the
organization's settings or the node's credential) and opens the mailbox read-only.

- After every (re)connect the trigger is checked once, for mail that arrived meanwhile.
- IDLE is renewed every 25 minutes (servers may drop idle clients after 30). A connection that
  does not answer the renewal is replaced.
- Lost connections are re-established with exponential backoff (1 s doubling up to 5 min,
  with jitter).
- A server without `IDLE`: the trigger polls and tries again after an hour.
- Each replica keeps at most `G2F_IMAP_IDLE_MAX` (default 50) IDLE connections; triggers
  beyond that on every replica poll. Mail servers also limit connections per account (Gmail:
  15), so many triggers on one mailbox may need *Polling only* on some of them.
- Changing the trigger or the Email settings reconnects; deactivating the workflow closes the
  connection.

### Gmail

Gmail sends notifications through Google Cloud Pub/Sub. One-time setup by a Google Cloud
project owner and an organization admin:

1. In the Google Cloud project of the OAuth client: enable the **Cloud Pub/Sub API**.
2. Create a topic, e.g. `projects/my-project/topics/gmail-push`.
3. Grant `gmail-api-push@system.gserviceaccount.com` the **Pub/Sub Publisher** role on the
   topic (this lets Gmail publish to it).
4. In glue2flow, *Plugins → Gmail → Settings*: set **Pub/Sub topic for push** to the full
   topic name.
5. Open any Gmail New Email trigger as an organization admin: its info box shows the
   **Gmail push endpoint** of your organization, `<G2F_PUBLIC_URL>/push/gmail/<token>`.
6. Create a **push** subscription on the topic with that endpoint URL. Optional but
   recommended: enable authentication on it with a service account of your project, and enter
   that service account's e-mail as **Push service account** in the Gmail settings.

glue2flow then calls `users.watch` for every connected account that has an active Gmail
trigger (one watch per account, shared by its triggers), renews it daily (watches expire
after 7 days) and calls `users.stop` when the account's last trigger goes away. A
notification names only the mailbox address: glue2flow finds the accounts with that address
(the account name shown on the *Connections* page) and checks their active triggers. The watch
covers the whole mailbox (Gmail search queries cannot be expressed as watch labels), so any
change may cause a check; the check decides what is new.

Authentication of the endpoint: the unguessable token in the path identifies the organization
(unknown tokens get 404). It is a secret: anyone who knows it can make glue2flow *check* the
organization's Gmail triggers (nothing else, and checks are coalesced). With a push service
account configured, every request must also carry a Google-signed OIDC token of that account
for exactly this endpoint URL (401 otherwise); this needs outbound access to Google's public
keys. Well-formed authenticated messages are always answered 204, whether or not the address
belongs to a trigger.

### Outlook (Microsoft Graph)

Nothing to set up beyond a public https `G2F_PUBLIC_URL`: the delegated `Mail.ReadWrite`
permission the Outlook module already requests covers mail subscriptions. For each active
trigger glue2flow creates a subscription on the trigger's folder
(`me/mailFolders('<folder>')/messages`, change type `created`) with a random `clientState`
(stored encrypted), answering Graph's validation handshake on `POST /push/graph`. It renews
the subscription daily and always well before it expires (mail subscriptions last at most
about 3 days), deletes it when the trigger goes away and replaces it when the folder changes.
Notifications whose `clientState` does not match are dropped. Lifecycle notifications are
handled: *reauthorizationRequired* renews, *subscriptionRemoved* checks and subscribes again,
*missed* checks.

### Replicas

Any replica can receive notifications. Subscriptions at Google and Microsoft are created,
renewed and removed by one replica at a time (the *push reconciler*, a lease in PostgreSQL),
which compares the active triggers with the stored subscriptions (`push_subscriptions`) every
30 seconds and right after a workflow is saved on its replica. Failed calls are retried per
subscription with backoff (1 minute doubling up to 1 hour), so a broken account does not
hammer the provider.

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
  IP 20 (see `G2F_TRUSTED_PROXIES` behind a reverse proxy), and a form 200 in total.
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

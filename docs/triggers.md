[Docs](README.md) › Triggers

# Triggers

Every workflow starts at a trigger node. A workflow may have several triggers.

| Trigger | Fires when | Item emitted |
| --- | --- | --- |
| **Manual** | a user with role `runner`+ clicks ▶ Run, or `POST /api/workflows/{id}/run` | the node's test payload (or `items` from the request body) |
| **Schedule** | the workflow is **active** and the time is due | `{firedAt, scheduledAt, timezone}` |
| **Webhook** | the workflow is **active** and `/webhook/{workflowId}/{nodeId}` is called | `{method, headers, query, body, receivedAt}` |
| **Called by Workflow** | an [Execute Workflow](nodes.md#execute-workflow) node of another workflow calls it | the caller's items |
| **Error Trigger** | a watched workflow fails | `{workflow, execution, failedNode, error}` |

Any trigger can also be run manually from the editor (select it, then ▶ Run). Webhook and
schedule triggers then use their test payload, so a flow can be tested before activation.

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

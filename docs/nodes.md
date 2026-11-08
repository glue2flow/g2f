[Docs](README.md) › Built-in nodes

# Built-in nodes

| Category | Nodes |
| --- | --- |
| Triggers | Manual, Schedule, Webhook, Called by Workflow, Error Trigger (see [Triggers](triggers.md)) |
| Core | [HTTP Request](#http-request), [Set Fields](#set-fields), [If](#if), [Switch](#switch), [Merge](#merge), [Code](#code), [Wait](#wait), [Transform Items](#transform-items), [Log](#log) |
| Flow | [Execute Workflow](#execute-workflow) (incl. loops over items / batches) |
| Logic | [And, Or, Xor](#logic-gates) |

More nodes come from [plugins](plugins.md): GitHub, Slack and Google Sheets ship in `modules/`,
Telegram and Mistral AI in [`examples/plugins`](../examples/plugins).

Every node type declares the fields it outputs ([output schema](plugins.md#output-schema)), so the
⚡ picker can offer them in later nodes before the workflow has run.

## HTTP Request

Calls any HTTP API, once per input item. Method, URL, headers, query and body accept
expressions. Outputs `{status, headers, body}`; JSON bodies are parsed. By default the node
fails on HTTP status ≥ 400.

## Set Fields

Adds or overwrites fields on each item. Values may use `{{ expressions }}`. With
*Keep existing fields* off, only the configured fields are emitted.

## If

Routes each item to the `true` or `false` output by a condition such as
`{{ json.status == 200 }}`.

## Switch

Routes each item by value: `value: {{ json.severity }}`, `cases: critical, warning`
creates the outputs `critical`, `warning` and `default`.

## Merge

Combines inputs A and B:
- *append*: A, then B;
- *join by key*: `{{ json.id }}` on both sides, keeping inner, left or outer matches;
- *pair by position*.

Fields of B win on conflicts.

## Code

Runs JavaScript in an embedded, sandboxed interpreter ([goja](https://github.com/dop251/goja)).
There is no network, file system, modules or timers, every run uses a fresh VM, and runs stop
after a timeout (default 10 s).
- *Once for all items*: use `items`, return an array.
- *Once per item*: use `item` and `index`, return an object, or `null` to drop the item.

`nodes`, `trigger`, `workflow`, `execution` and `console.log()` (also `info`, `warn`, `error`,
`debug`) are available. Console output goes to the run's execution log. Administrators
can turn the node off with `G2F_DISABLE_CODE_NODE=true`. Memory use of scripts is not limited.

```js
return items.filter(i => i.total > 100).map(i => ({ id: i.id, vip: true }));
```

## Wait

Pauses an execution and stores its complete state in PostgreSQL, so waits of
minutes, days or weeks survive restarts and deployments. Three modes:
- *after an interval*;
- *at a specific time* (with timezone);
- *when the resume URL is called*: `{{ execution.resumeUrl }}` can be used in any node before
  the Wait, e.g. in an approval message. Calling it with any HTTP method continues the run, and
  the request (`query`, `body`, `headers`) becomes the Wait node's output.

Unanswered webhook waits continue on the `timeout` output after the configured hours. Resume
URLs are HMAC-signed with a key derived from `G2F_SECRET_KEY`. Waiting runs show a *Cancel*
button, and the execution timeout only counts active time. Every replica polls for due waits;
each resume is claimed atomically, so it happens exactly once.

```
Expense ─► Ask manager (Slack: "{{ execution.resumeUrl }}?decision=approve") ─► Wait ─► Switch(json.query.decision)
                                                                                   └─timeout─► Escalate
```

## Transform Items

Evaluates one expression over all input items (variable `items`) and emits the result.
Arrays become multiple items, e.g. `{{ filter(items, .active) }}` or
`{{ map(items, ({name: .login})) }}`.

## Log

Writes its *Message* (multi-line, with `{{ expressions }}`) once per item at the chosen
level (debug, info, warning, error) and passes the items on unchanged. Messages appear in the
editor under the run: the **Log** tab of the Executions panel shows all messages of a run, and
each node's tab shows its own. They also go to the server log with the workflow and execution
IDs. Known secret values are masked. A node keeps at most 500 messages of up to 8 KB each per run.

## Execute Workflow

Runs another workflow that starts with a *Called by Workflow* trigger, and
continues with that workflow's last output. Modes:
- *once* with all items;
- *per item*;
- *per batch of N*, with an optional pause between runs.

The per-item and batch modes are how glue2flow loops: the engine runs acyclic graphs, and a loop body
becomes a reusable sub-workflow. Each call is its own execution (`triggered by workflow: …`).
Calls stay within the organization and nest at most 5 levels deep. Saving requires the
`runner` role on the called workflow. Wait nodes are not allowed in called workflows.

## Logic gates

Combine two branches. Each gate has the inputs **A** and **B** and the outputs
`true` / `false`. An input is *true* when it received at least one item matching its
condition (default `{{ true }}`, i.e. "received anything"). Example: `{{ json.status == 'ok' }}`.

| Gate | `true` when | items on `true` |
| --- | --- | --- |
| And | A and B | items of A and B |
| Or | A or B | items of the true inputs |
| Xor | exactly one of A, B | items of the true input |

All received items go to `false` otherwise. Because a node runs only after all its
predecessors, *And* also works as a join: "continue when both branches produced data".

## Writing nodes in Go

For logic that does not fit a plugin, implement the `nodes.Node` interface in Go (see
`internal/nodes/builtin.go`), declare an `OutputSchema` in its definition, and register it in
`RegisterBuiltins`. A test fails for built-in nodes without an output schema.

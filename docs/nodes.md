[Docs](README.md) › Built-in nodes

# Built-in nodes

| Category | Nodes |
| --- | --- |
| Triggers | Manual, Schedule, Webhook, Form, Called by Workflow, Error Trigger, polling triggers such as Email Received (see [Triggers](triggers.md)) |
| Core | [HTTP Request](#http-request), [Set Fields](#set-fields), [Condition](#condition), [Switch](#switch), [Merge](#merge), [Code](#code), [Wait](#wait), [Transform Items](#transform-items), [Log](#log) |
| Data | [Filter](#filter), [Sort](#sort), [Limit](#limit), [Split Out](#split-out), [Aggregate](#aggregate), [Map](#map), [Rename / Remove Fields](#rename--remove-fields), [Date & Time](#date--time), [CSV](#csv), [Remove Duplicates](#remove-duplicates), [Data Store](#data-store) |
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

## Condition

Routes each item to the `true` or `false` output by a condition such as
`{{ json.status == 200 }}` (if / else). Its type is `core.if`; it was called *If* before.

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
after a timeout (default 10 s) or when they use more memory than allowed (default 128 MB).
- *Once for all items*: use `items`, return an array.
- *Once per item*: use `item` and `index`, return an object, or `null` to drop the item.

`nodes`, `trigger`, `workflow`, `execution` and `console.log()` (also `info`, `warn`, `error`,
`debug`) are available. Console output goes to the run's execution log. Administrators
can turn the node off with `G2F_DISABLE_CODE_NODE=true`.

**Isolation.** Each run happens in its own short-lived process (the `g2f` binary started
again, about 10 ms), so a script cannot take the server down:

- **Memory:** a watchdog in the sandbox stops the script when its heap exceeds
  `G2F_CODE_MEMORY_MB` (default 128). The run fails with *code exceeded its memory limit*.
  On Linux the server additionally kills a sandbox whose resident memory grows far beyond the limit.
- **Secrets:** the sandbox starts with an empty environment: no `G2F_SECRET_KEY`, no database URL.
- **Crashes:** on Linux the sandbox dies with the server, so no script outlives it.

`G2F_CODE_MEMORY_MB=0` runs scripts inside the server process without a memory limit
(the behaviour of earlier versions).

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

## Filter

Keeps the items whose condition is true, e.g. `{{ json.total > 100 }}`. The others leave
through the `discarded` output, which can stay unconnected.

## Sort

Sorts by the value of an expression, ascending or descending. Numbers (also numeric text) sort
numerically, ISO dates chronologically, text alphabetically ignoring case; empty values come
last. To sort by several values, return an array: `{{ [json.lastName, json.firstName] }}`.

## Limit

Keeps the first or last N items.

## Split Out

Turns a list inside each item into separate items: `{{ json.lines }}` makes one item per order
line. Each new item is the list element, or a copy of the original item with the element in a
field (*Field name*, default `item`). Elements that are not objects are put into that field.

## Aggregate

Combines items into one, or one per group (*Group by*, e.g. `{{ json.customerId }}`):

| Operation | Result |
| --- | --- |
| Collect the items | the items as a list |
| Collect a value / Unique values | the values of *Value* (e.g. `{{ json.email }}`), all or each once |
| Count, Sum, Average, Minimum, Maximum | numbers (min/max also work for text and dates); empty values are skipped |
| Join as text | the values joined with *Separator* |
| First / Last value | |

Each output item has the result (field *Result field*, default `result`), `count`, and with
grouping `group`. Typical: *Split Out* order lines → *Aggregate* sum of `{{ json.qty }}` grouped
by `{{ json.sku }}`.

## Map

Builds a new item from each item with one expression:
`{{ {name: json.firstName + ' ' + json.lastName, email: lower(json.email)} }}`. Returning a list
emits several items, `nil` drops the item. Unlike [Transform Items](#transform-items) it runs
once per item.

## Rename / Remove Fields

Renames fields (`{"mail": "email", "address.zip": "postalCode"}`), removes fields
(`password, internal.notes`) or keeps only the listed ones. Paths may be nested.

## Date & Time

Works with dates from any source: ISO 8601 text, `2026-03-01`, `2026-03-01 14:05`, e-mail dates
(`Sun, 01 Mar 2026 14:05:00 +0100`), Unix seconds or milliseconds, or `now()`.

| Operation | Result |
| --- | --- |
| Format | the date in a preset or custom format |
| Add / Subtract time | seconds … years; whole days keep the time of day across daylight saving changes |
| Start / End of a period | hour, day, week (Monday), month, quarter, year |
| Difference | end minus start, in the chosen unit (months/years in whole calendar units) |
| Parse with a custom format | reads text like `28.03.2026 9:05` with `dd.MM.yyyy H:mm` |

The *Timezone* (default UTC) applies to the output, to periods and to dates without a zone.
Custom formats use the tokens `yyyy yy MMMM MMM MM M dd d EEEE EEE HH H hh h mm m ss s SSS a Z ZZ z`;
put other letters in single quotes: `d. MMMM yyyy 'um' HH:mm`. The result goes into *Result
field* (default `date`); the other fields of the item are kept.

## CSV

- *CSV text → items*: one item per row, named by the header row (or `column1`, `column2`, …).
  The delimiter is detected (`,` `;` tab `|`), quotes and Excel's byte order mark are handled,
  and base64 content (an e-mail attachment) is decoded first.
- *Items → CSV file*: one item with `csv` (text), `content` (base64), `filename` and
  `contentType`, ready for the `attachments` of a Send Email node. *Columns* picks and orders
  fields (nested paths allowed); by default all fields are used.

## Remove Duplicates

Lets each item through once. *Compare by* decides what counts as the same item
(`{{ json.id }}`, or `{{ json }}` for whole items); repeats go to the `duplicates` output.

- *Within this run only*: duplicates among the input items.
- *Across runs*: items seen in earlier runs are duplicates too. The history is stored in
  PostgreSQL for each workflow (hashes of the keys, not the items), and kept for *Forget after
  (days)* (default 30, 0 = forever). Nodes with the same *History name* share it. Parallel runs
  never both let the same item through.

Manual test runs show what would pass but do not change the history.

```
Schedule (every 15 min) ─► HTTP Request (GET /orders) ─► Split Out (json.orders)
   ─► Remove Duplicates (json.id, across runs) ─► Send Email "New order {{ json.id }}"
```

## Data Store

Keeps values between runs of the workflow, e.g. a counter, the last processed ID or a cached
token. Operations: get (with a default), set (any JSON value, optionally expiring after N
days), increment (atomic, also across parallel runs), delete, list (by key prefix) and delete
all. Get/set/increment/delete run once per item and put the value into *Result field* (default
`value`). Values belong to the workflow and are deleted with it; each is at most 256 KB.

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

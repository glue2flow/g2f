[Docs](README.md) › Expressions

# Expressions

Any parameter can contain `{{ ... }}` expressions in the [expr language](https://expr-lang.org/docs/language-definition):

| Variable | Meaning |
| --- | --- |
| `json` | the current item |
| `items` | all input items of the node |
| `nodes['Node name']` | output items of an earlier node |
| `trigger` | the first item emitted by the trigger |
| `workflow.id`, `workflow.name`, `execution.id` | run metadata |
| `execution.resumeUrls['Node name']` | resume URL of that [Wait](nodes.md#wait) node (resumes only that node) |
| `execution.resumeUrl` | the resume URL, only if the workflow has exactly one Wait node |
| `vars` | the plugin's organization variables (plugin nodes only, see [Secrets](secrets.md)) |
| `params` | resolved node parameters (YAML modules only) |

Member access is null-safe: `{{ trigger.body.name }}` is `nil` when `body` is missing, so
`{{ trigger.body.name ?? 'anonymous' }}` works as a default.
A parameter that is exactly one expression keeps its type (`{{ json.count + 1 }}` → number);
mixed text yields a string. Inside `map`/`filter`, wrap object literals in parentheses:
`{{ map(items, ({id: .id})) }}`.

Keys that are not plain identifiers use brackets: `{{ json.user['first name'] }}`.

Besides the expr builtins (`toBase64`, `fromJSON`, `map`, …) there are a few helpers:

| Function | Result |
| --- | --- |
| `toBase64Url(s)` / `fromBase64Url(s)` | URL-safe base64 (fromBase64Url also takes standard base64; nil → `""`) |
| `emailAddresses(v)` | `"a@x.com, \"Doe, Jane\" <j@x.com>"` or an array → `[{name, address}]` |
| `mimeMessage({from, to, cc, bcc, replyTo, subject, text, html, headers, attachments})` | a complete RFC 5322 e-mail (UTF-8, multipart when needed); attachments are `[{filename, content (base64), contentType}]` |

## Dynamic content (⚡)

You don't have to type paths. Every field that accepts expressions has a ⚡ button, like
Power Automate's dynamic content. It lists:

- **Current item** (`json`): fields of the directly connected node;
- **every upstream node**, closest first (`nodes['Name'][0]`);
- the **trigger**, the plugin's **organization variables**, and **workflow** metadata.

Clicking a field inserts the expression at the cursor: `{{ json.text }}` for the directly
connected node, `{{ nodes['HTTP Request'][0].body.id }}` for earlier ones. Inside an open
`{{ }}` only the path is inserted. In JSON fields the expression is quoted, so the field stays
valid JSON. In Code fields it is inserted without braces, with `item` or `items[0]` in place
of `json`.

The fields come from two sources:

1. the **latest run** (or the run selected in the Executions panel), with sample values;
2. each node type's **[output schema](plugins.md#output-schema)**, so nodes that have not run yet
   are listed too. Pass-through nodes (Condition, Switch, Log, …) inherit the fields of their inputs.

`nodes['Name'][0]` is the *first* item of that node. To work with every item of an earlier
node, use `nodes['Name']` in a [Transform Items](nodes.md#transform-items) or [Code](nodes.md#code) node.

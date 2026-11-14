# glue2flow documentation

Everything about running, using and extending glue2flow. New here? Start with
**[Getting started](getting-started.md)**.

## Using glue2flow

| Page | What's inside |
| --- | --- |
| [Getting started](getting-started.md) | Docker quick start, demo users, local development, your first workflow |
| [Built-in nodes](nodes.md) | HTTP Request, Set Fields, Condition, Switch, Merge, Code, Wait, Log, Execute Workflow, logic gates; data nodes (Filter, Sort, Split Out, Aggregate, Date & Time, CSV, …); Remove Duplicates and Data Store |
| [Triggers](triggers.md) | Manual, Schedule (cron / once), Webhook (auth, responses), Form (public web form), Called by Workflow, Error Trigger, polling triggers (new e-mail) |
| [Expressions](expressions.md) | `{{ }}` syntax, variables, the ⚡ dynamic content picker |
| [Sharing & Keycloak](sharing.md) | Scopes, roles, groups, organizations, required token claims |
| [Connections](connections.md) | OAuth2 accounts (Gmail, Outlook, Teams, Google Sheets, Calendar and Drive, HubSpot, Salesforce, SharePoint: connecting, token storage and refresh) and API-key credentials (GitHub, Slack, Email) |
| [Email (SMTP/IMAP)](email.md) | Sending and reading mail with any mail server |

## Operating glue2flow

| Page | What's inside |
| --- | --- |
| [Configuration](configuration.md) | All `G2F_*` environment variables, health check |
| [Secrets & plugin settings](secrets.md) | Per-node secrets, plugin config vs. vars, encryption, key rotation |
| [Architecture](architecture.md) | Repository layout, execution model, running several replicas |

## Extending glue2flow

| Page | What's inside |
| --- | --- |
| [Plugins](plugins.md) | Included plugins, installing, WebAssembly plugins, native Go modules, manifest reference, output schemas |
| [HTTP modules](http-modules.md) | Wrapping a REST API in a few lines of YAML |
| [Telegram plugin](../examples/plugins/telegram/README.md) | Complete WebAssembly sample in Go and JavaScript, calling convention |
| [Mistral AI plugin](../examples/plugins/mistral/README.md) | Compact Go plugin: prompt in, LLM answer out |

## All pages

- [Getting started](getting-started.md)
- [Built-in nodes](nodes.md)
- [Triggers](triggers.md)
- [Expressions](expressions.md)
- [Sharing & Keycloak](sharing.md)
- [Connections (OAuth2 accounts, API-key credentials)](connections.md)
- [Email (SMTP/IMAP)](email.md)
- [Secrets & plugin settings](secrets.md)
- [Configuration](configuration.md)
- [Architecture](architecture.md)
- [Plugins](plugins.md)
- [HTTP modules](http-modules.md)

Back to the [project page](../README.md).

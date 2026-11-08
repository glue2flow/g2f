[Docs](README.md) › Getting started

# Getting started

## Quick start (Docker)

```bash
docker compose up --build
```

| URL | What | Login |
| --- | --- | --- |
| http://localhost:8080 | glue2flow editor | `alice`/`alice` (org admin + plugin admin, /engineering/backend), `bob`/`bob` (/engineering/frontend), `carol`/`carol` (/sales), `dave`/`dave` (other org) |
| http://localhost:8081 | Keycloak admin | `admin`/`admin` |

The compose file is a demo: default passwords, Keycloak in dev mode, plain HTTP.
For a real deployment, see [Configuration](configuration.md) and [Sharing & Keycloak](sharing.md#keycloak-setup).

## Local development

```bash
make dev-db          # PostgreSQL in Docker on :5432
make dev-server      # Go API on :8080 with auth disabled (G2F_AUTH_MODE=none)
make dev-web         # Vite dev server on :5173, proxies /api and /webhook to :8080
make test            # go vet, go test, TypeScript typecheck
make test-db         # all Go tests incl. PostgreSQL ones (recovery, state store, polling triggers), with the race detector
make plugin-sample   # build examples/plugins/telegram/telegram.zip
make plugin-mistral  # build examples/plugins/mistral/mistral.zip
```

With `G2F_AUTH_MODE=none` every request runs as the user `dev` in the organization `default`.
Never use it outside of development.

## Your first workflow

1. Open the editor and create a workflow.
2. Drag a **Manual Trigger** onto the canvas and set its *Test payload*, e.g. `{"name": "Ada"}`.
3. Add a **Set Fields** node, connect the two, and set a field to `Hello {{ json.name }}`.
   The ⚡ button next to the field lists the trigger's fields ([Expressions](expressions.md#dynamic-content-)).
4. Click **▶ Run**. The Executions panel below the canvas shows every node's output.
5. Swap the manual trigger for a [Schedule or Webhook trigger](triggers.md) and switch the
   workflow to **Active**.

Next: [Built-in nodes](nodes.md) · [Expressions](expressions.md) · [Plugins](plugins.md)

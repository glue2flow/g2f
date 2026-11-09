[Docs](README.md) › Configuration

# Configuration

glue2flow is configured with environment variables.

| Variable | Default | Description |
| --- | --- | --- |
| `G2F_LISTEN` | `:8080` | listen address |
| `G2F_DATABASE_URL` | `postgres://g2f:g2f@localhost:5432/g2f?sslmode=disable` | PostgreSQL URL |
| `G2F_PUBLIC_URL` | `http://localhost:8080` | external URL (used for webhook, resume and OAuth callback URLs) |
| `G2F_SECRET_KEY` | – (required) | base64 32-byte key encrypting secrets ([Secrets](secrets.md#encryption)) |
| `G2F_SECRET_KEYS_OLD` | – | previous keys (comma separated), for rotation |
| `G2F_AUTH_MODE` | `oidc` | `oidc` or `none` (development only) |
| `G2F_OIDC_ISSUER` | – | expected token issuer, e.g. `https://sso.example.com/realms/g2f` |
| `G2F_OIDC_JWKS_URL` | `<issuer>/protocol/openid-connect/certs` | where to fetch signing keys |
| `G2F_OIDC_TOKEN_URL` | `G2F_OIDC_JWKS_URL` with `/certs` → `/token`, else `<issuer>/protocol/openid-connect/token` | token endpoint for sign-in to [forms](triggers.md#form) |
| `G2F_OIDC_CLIENT_ID` | `g2f` | client id (checked against `azp`/`aud`) |
| `G2F_OIDC_GROUPS_CLAIM` | `groups` | see [Keycloak setup](sharing.md#keycloak-setup) |
| `G2F_OIDC_ORG_CLAIM` | `org` | |
| `G2F_OIDC_ADMIN_ROLE` | `g2f-admin` | organization admin |
| `G2F_OIDC_PLUGIN_ADMIN_ROLE` | `g2f-plugin-admin` | may import plugins (installation-wide) |
| `G2F_DEFAULT_ORG` | `default` | org for users without an org claim |
| `G2F_DEV_USER` | `dev` | user name with `G2F_AUTH_MODE=none` |
| `G2F_DEV_GROUPS` | `/engineering` | groups of the dev user |
| `G2F_WORKERS` | `16` | max concurrent executions |
| `G2F_EXECUTION_TIMEOUT` | `5m` | per execution (active time; waiting does not count; restarts after a takeover) |
| `G2F_MAX_STORED_ITEMS` | `100` | items stored per node output |
| `G2F_SCHEDULE_CATCHUP` | `1h` | how late a missed one-time schedule may still run |
| `G2F_WEBHOOK_MAX_WAIT` | `60s` | max wait for webhooks that respond with the result |
| `G2F_STATIC_DIR` | `./web/dist` | built editor |
| `G2F_MODULES_DIR` | `./modules` | plugins loaded at startup |
| `G2F_PLUGIN_SYNC` | `30s` | how often imported plugins are reloaded from the database |
| `G2F_DISABLE_CODE_NODE` | `false` | turn off the JavaScript Code node |
| `G2F_CODE_MEMORY_MB` | `128` | memory limit of Code node scripts; `0` runs them in the server process without a limit ([Code](nodes.md#code)) |
| `G2F_INSTANCE_TIMEOUT` | `30s` | how long an instance may miss heartbeats before others take over its executions ([replicas](architecture.md#scaling-and-replicas)) |
| `G2F_SHUTDOWN_GRACE` | `15s` | on shutdown, how long running executions may finish before they are handed to another instance |

## Health check

The binary has a built-in health check for container orchestrators:

```bash
g2f healthcheck
```

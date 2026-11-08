[Docs](README.md) › Sharing & Keycloak

# Sharing & Keycloak

## Sharing model

Each workflow has an **owner**, an **organization** and a **scope**:

| Scope | Who can access |
| --- | --- |
| `private` | owner only |
| `group` | owner + members of the listed Keycloak groups (subgroups included: a share for `/engineering` also covers `/engineering/backend`) |
| `org` | everyone in the organization with the default `orgRole`; group shares can grant more |

Roles: `viewer` (see workflow and runs) < `runner` (+ run manually) < `editor` (+ edit, activate)
< `owner` (+ delete, change sharing). Users with the realm role `g2f-admin` are owners of every
workflow in their organization. Workflows are never visible across organizations.
Secret parameters are never returned by the API, whatever the role (see [Secrets](secrets.md)).

## Keycloak setup

glue2flow reads these claims from the access token (claim names are configurable):

| Claim | Source in Keycloak | Env var |
| --- | --- | --- |
| `groups` | *Group Membership* mapper with **full group path** on | `G2F_OIDC_GROUPS_CLAIM` |
| `org` | *User Attribute* mapper for attribute `org`, or the `organization` claim of Keycloak Organizations (both formats supported) | `G2F_OIDC_ORG_CLAIM` |
| `realm_access.roles` | realm role `g2f-admin` (organization admin) and `g2f-plugin-admin` (may import plugins) | `G2F_OIDC_ADMIN_ROLE`, `G2F_OIDC_PLUGIN_ADMIN_ROLE` |

[`deploy/keycloak/realm-g2f.json`](../deploy/keycloak/realm-g2f.json) has a complete example: a
public client `g2f` with PKCE, both mappers, groups, and demo users.

Other OIDC providers work as long as they put groups and the organization into the access
token; see the `G2F_OIDC_*` variables in [Configuration](configuration.md).

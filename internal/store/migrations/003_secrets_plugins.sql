-- Encrypted per-node secret parameters. Never part of workflows.graph.
CREATE TABLE node_secrets (
    workflow_id uuid        NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    node_id     text        NOT NULL,
    param       text        NOT NULL,
    value       text        NOT NULL, -- secrets.Box sealed
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, node_id, param)
);

-- Global plugin configuration, per organization (e.g. OAuth client secret).
CREATE TABLE plugin_config (
    org_id     text        NOT NULL,
    module     text        NOT NULL,
    key        text        NOT NULL,
    value      text        NOT NULL, -- secrets.Box sealed (all values, secret or not)
    updated_by text        NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, module, key)
);

-- Imported plugins (YAML manifests and WASM bundles), shared by all replicas.
CREATE TABLE plugins (
    module       text PRIMARY KEY,
    version      text        NOT NULL DEFAULT '',
    manifest     text        NOT NULL,
    wasm         bytea,
    sha256       text        NOT NULL,
    installed_by text        NOT NULL DEFAULT '',
    installed_at timestamptz NOT NULL DEFAULT now()
);

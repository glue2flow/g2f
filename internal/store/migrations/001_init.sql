CREATE TABLE workflows (
    id          uuid PRIMARY KEY,
    org_id      text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    owner_sub   text        NOT NULL,
    owner_name  text        NOT NULL DEFAULT '',
    scope       text        NOT NULL CHECK (scope IN ('private', 'group', 'org')),
    org_role    text        NOT NULL DEFAULT 'viewer',
    -- [{"group": "/engineering", "role": "editor"}, ...]
    shares      jsonb       NOT NULL DEFAULT '[]',
    graph       jsonb       NOT NULL,
    active      boolean     NOT NULL DEFAULT false,
    version     integer     NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workflows_org_idx ON workflows (org_id, updated_at DESC);
CREATE INDEX workflows_owner_idx ON workflows (owner_sub);
CREATE INDEX workflows_shares_idx ON workflows USING gin (shares jsonb_path_ops);

CREATE TABLE executions (
    id               uuid PRIMARY KEY,
    workflow_id      uuid        NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    workflow_version integer     NOT NULL,
    trigger_type     text        NOT NULL,
    trigger_node     text        NOT NULL DEFAULT '',
    triggered_by     text        NOT NULL DEFAULT '',
    status           text        NOT NULL,
    error            text        NOT NULL DEFAULT '',
    steps            jsonb       NOT NULL DEFAULT '[]',
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);

CREATE INDEX executions_workflow_idx ON executions (workflow_id, started_at DESC);
CREATE INDEX executions_status_idx ON executions (status) WHERE status IN ('queued', 'running');

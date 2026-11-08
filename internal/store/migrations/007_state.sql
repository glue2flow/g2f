-- Durable state of workflows: the Data Store node's values, Remove Duplicates
-- history and the cursors of polling triggers. Rows are scoped to a workflow
-- and removed with it.
CREATE TABLE workflow_state (
    workflow_id uuid        NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    scope       text        NOT NULL, -- e.g. 'data', 'dedupe:<name>', 'poll:<node id>'
    key         text        NOT NULL,
    value       jsonb       NOT NULL DEFAULT 'null',
    updated_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    PRIMARY KEY (workflow_id, scope, key)
);

CREATE INDEX workflow_state_expires_idx ON workflow_state (expires_at) WHERE expires_at IS NOT NULL;

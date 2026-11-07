-- One row per trigger firing. The primary key makes firing idempotent, so every
-- replica can run the scheduler and exactly one of them wins each firing.
CREATE TABLE trigger_firings (
    workflow_id uuid        NOT NULL REFERENCES workflows (id) ON DELETE CASCADE,
    node_id     text        NOT NULL,
    fire_key    text        NOT NULL,
    fired_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, node_id, fire_key)
);

CREATE INDEX trigger_firings_fired_at_idx ON trigger_firings (fired_at);

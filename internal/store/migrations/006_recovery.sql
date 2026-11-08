-- Executions survive the instance that runs them: every glue2flow instance
-- registers itself and sends heartbeats; running executions belong to one
-- instance (owner) and are checkpointed (state) after every node. When an
-- instance stops sending heartbeats, another one takes its executions over.
CREATE TABLE engine_instances (
    id         uuid PRIMARY KEY,
    hostname   text        NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL DEFAULT now(),
    last_seen  timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE executions
    ADD COLUMN owner    uuid,
    -- How often the execution was taken over after its instance stopped.
    ADD COLUMN attempts integer NOT NULL DEFAULT 0;

CREATE INDEX executions_owner_idx ON executions (owner) WHERE status IN ('queued', 'running');

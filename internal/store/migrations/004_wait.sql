-- Paused executions (Wait node): the engine snapshot and when to resume.
ALTER TABLE executions
    ADD COLUMN state     jsonb,
    ADD COLUMN resume_at timestamptz,
    ADD COLUMN wait_mode text NOT NULL DEFAULT '';

CREATE INDEX executions_resume_idx ON executions (resume_at) WHERE status = 'waiting';

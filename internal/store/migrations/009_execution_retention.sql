-- Finished executions are deleted after a retention period (G2F_EXECUTION_RETENTION).
CREATE INDEX executions_finished_idx ON executions (finished_at) WHERE status IN ('success', 'error');

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ExecStatus string

const (
	StatusQueued  ExecStatus = "queued"
	StatusRunning ExecStatus = "running"
	StatusWaiting ExecStatus = "waiting"
	StatusSuccess ExecStatus = "success"
	StatusError   ExecStatus = "error"
)

// Step records the run of one node.
type Step struct {
	NodeID     string                      `json:"nodeId"`
	NodeName   string                      `json:"nodeName"`
	NodeType   string                      `json:"nodeType"`
	Status     string                      `json:"status"` // success | error | skipped
	StartedAt  time.Time                   `json:"startedAt"`
	DurationMs int64                       `json:"durationMs"`
	InputCount int                         `json:"inputCount"`
	Output     map[string][]map[string]any `json:"output,omitempty"`
	Truncated  bool                        `json:"truncated,omitempty"`
	Error      string                      `json:"error,omitempty"`
	// Logs holds messages the node wrote to the execution log (Log node, console.log).
	Logs []LogEntry `json:"logs,omitempty"`
}

// LogEntry is one message in a step's execution log.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"` // debug | info | warn | error
	Message string    `json:"message"`
}

type Execution struct {
	ID              string     `json:"id"`
	WorkflowID      string     `json:"workflowId"`
	WorkflowVersion int        `json:"workflowVersion"`
	TriggerType     string     `json:"triggerType"`
	TriggerNode     string     `json:"triggerNode"`
	TriggeredBy     string     `json:"triggeredBy"`
	Status          ExecStatus `json:"status"`
	Error           string     `json:"error,omitempty"`
	Steps           []Step     `json:"steps,omitempty"`
	ResumeAt        *time.Time `json:"resumeAt,omitempty"`
	WaitMode        string     `json:"waitMode,omitempty"`
	StartedAt       time.Time  `json:"startedAt"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	// Attempts counts how often another instance took the execution over
	// after the instance running it stopped.
	Attempts int `json:"attempts,omitempty"`
	// Owner is the engine instance running the execution. Writes of other
	// instances are rejected (ErrNotOwner), so a taken-over execution cannot
	// be continued twice.
	Owner string `json:"-"`
}

// ErrNotOwner is returned when an execution is no longer owned by the
// instance writing it (it was taken over).
var ErrNotOwner = errors.New("execution is owned by another instance")

// nullUUID converts "" to NULL.
func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) CreateExecution(ctx context.Context, e *Execution) error {
	e.ID = uuid.NewString()
	if e.Status == "" {
		e.Status = StatusQueued
	}
	return s.pool.QueryRow(ctx, `INSERT INTO executions
		(id, workflow_id, workflow_version, trigger_type, trigger_node, triggered_by, status, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING started_at`,
		e.ID, e.WorkflowID, e.WorkflowVersion, e.TriggerType, e.TriggerNode, e.TriggeredBy, e.Status, nullUUID(e.Owner),
	).Scan(&e.StartedAt)
}

// UpdateExecution stores the final (or current) result and releases the
// execution: the snapshot and the owner are cleared. Returns ErrNotOwner if
// another instance took it over.
func (s *Store) UpdateExecution(ctx context.Context, e *Execution) error {
	steps, err := json.Marshal(e.Steps)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status=$2, error=$3, steps=$4, finished_at=$5,
		state=NULL, resume_at=NULL, wait_mode='', owner=NULL
		WHERE id=$1 AND ($6::uuid IS NULL OR owner IS NULL OR owner=$6)`,
		e.ID, e.Status, e.Error, steps, e.FinishedAt, nullUUID(e.Owner))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotOwner
	}
	return err
}

// Checkpoint stores the progress of a running execution with its engine
// snapshot, so another instance can continue it.
func (s *Store) Checkpoint(ctx context.Context, e *Execution, state []byte) error {
	steps, err := json.Marshal(e.Steps)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status=$2, steps=$3, state=$4,
		resume_at=NULL, wait_mode='' WHERE id=$1 AND owner=$5`, e.ID, e.Status, steps, state, nullUUID(e.Owner))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotOwner
	}
	return err
}

// ReleaseExecution hands an unfinished execution back (graceful shutdown):
// it is queued again with its last snapshot and taken over right away.
func (s *Store) ReleaseExecution(ctx context.Context, e *Execution, state []byte) error {
	steps, err := json.Marshal(e.Steps)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status='queued', steps=$2, state=$3, owner=NULL
		WHERE id=$1 AND owner=$4`, e.ID, steps, state, nullUUID(e.Owner))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotOwner
	}
	return err
}

// ReturnToWaiting puts an execution that was resumed from a Wait node, but
// stopped before it saved any progress, back into the waiting state.
func (s *Store) ReturnToWaiting(ctx context.Context, e *Execution) error {
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status='waiting', owner=NULL
		WHERE id=$1 AND owner=$2`, e.ID, nullUUID(e.Owner))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotOwner
	}
	return err
}

// Heartbeat registers the instance (or refreshes its last_seen time) and
// removes instances that have been gone for a day.
func (s *Store) Heartbeat(ctx context.Context, instance, hostname string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO engine_instances (id, hostname) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET last_seen=now()`, instance, hostname)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM engine_instances WHERE last_seen < now() - interval '1 day'`)
	return err
}

// RemoveInstance unregisters an instance that shut down.
func (s *Store) RemoveInstance(ctx context.Context, instance string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM engine_instances WHERE id=$1`, instance)
	return err
}

// Orphan is an execution taken over from a stopped instance.
type Orphan struct {
	Execution *Execution
	State     []byte // nil if the execution never saved progress
}

// ClaimOrphans takes over up to limit queued or running executions whose
// instance stopped:
//
//   - the owner sent no heartbeat for stale (or never registered);
//   - released executions (queued, no owner, with a snapshot) right away;
//   - executions without owner and snapshot (written by versions before
//     006_recovery) once they are older than legacyAge, the execution timeout
//     of those versions, so a rolling upgrade does not take over runs that an
//     old instance is still executing.
//
// Claiming is atomic (FOR UPDATE SKIP LOCKED): every execution goes to exactly
// one instance.
func (s *Store) ClaimOrphans(ctx context.Context, instance string, stale, legacyAge time.Duration, limit int) ([]Orphan, error) {
	rows, err := s.pool.Query(ctx, `UPDATE executions x SET owner=$1, attempts=x.attempts+1
		FROM (
			SELECT e.id FROM executions e
			LEFT JOIN engine_instances i ON i.id = e.owner
			WHERE e.status IN ('queued', 'running') AND (
				(e.owner IS NOT NULL AND i.id IS NOT NULL AND i.last_seen < now() - make_interval(secs => $2))
				OR (e.owner IS NOT NULL AND i.id IS NULL AND e.started_at < now() - make_interval(secs => $2))
				OR (e.owner IS NULL AND e.status = 'queued' AND e.state IS NOT NULL)
				OR (e.owner IS NULL AND e.state IS NULL AND e.started_at < now() - make_interval(secs => $3)))
			ORDER BY e.started_at
			LIMIT $4
			FOR UPDATE OF e SKIP LOCKED
		) c
		WHERE x.id = c.id
		RETURNING `+prefixed("x", execCols)+`, x.state`,
		instance, stale.Seconds(), legacyAge.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Orphan
	for rows.Next() {
		var state []byte
		e, err := scanExecution(rows, true, &state)
		if err != nil {
			return nil, err
		}
		e.Owner = instance
		out = append(out, Orphan{Execution: e, State: state})
	}
	return out, rows.Err()
}

const execCols = `id, workflow_id, workflow_version, trigger_type, trigger_node, triggered_by,
	status, error, steps, resume_at, wait_mode, started_at, finished_at, attempts`

// prefixed qualifies a column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// scanExecution reads execCols, followed by the given extra columns.
func scanExecution(row pgx.Row, withSteps bool, extra ...any) (*Execution, error) {
	var e Execution
	var id, wid uuid.UUID
	var steps []byte
	dest := append([]any{&id, &wid, &e.WorkflowVersion, &e.TriggerType, &e.TriggerNode, &e.TriggeredBy,
		&e.Status, &e.Error, &steps, &e.ResumeAt, &e.WaitMode, &e.StartedAt, &e.FinishedAt, &e.Attempts}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.ID, e.WorkflowID = id.String(), wid.String()
	if withSteps {
		if err := json.Unmarshal(steps, &e.Steps); err != nil {
			return nil, err
		}
	}
	return &e, nil
}

func (s *Store) GetExecution(ctx context.Context, id string) (*Execution, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return scanExecution(s.pool.QueryRow(ctx, `SELECT `+execCols+` FROM executions WHERE id=$1`, uid), true)
}

// ListExecutions returns the latest executions of a workflow without step data.
func (s *Store) ListExecutions(ctx context.Context, workflowID string, limit int) ([]*Execution, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+execCols+` FROM executions
		WHERE workflow_id=$1 ORDER BY started_at DESC LIMIT $2`, workflowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Execution{}
	for rows.Next() {
		e, err := scanExecution(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PauseExecution stores a waiting execution with its engine snapshot.
func (s *Store) PauseExecution(ctx context.Context, e *Execution, state []byte) error {
	steps, err := json.Marshal(e.Steps)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status='waiting', steps=$2, state=$3,
		resume_at=$4, wait_mode=$5, owner=NULL WHERE id=$1 AND ($6::uuid IS NULL OR owner IS NULL OR owner=$6)`,
		e.ID, steps, state, e.ResumeAt, e.WaitMode, nullUUID(e.Owner))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotOwner
	}
	return err
}

// ClaimWaiting atomically moves a waiting execution to running and returns it
// with its snapshot. With mode set, only executions waiting in that mode match;
// with nodeID set, only executions waiting at that Wait node. The node is
// compared in the same statement, so a mismatch claims nothing.
// Returns ErrNotFound if the execution is not (or no longer) waiting.
func (s *Store) ClaimWaiting(ctx context.Context, id, mode, nodeID, owner string) (*Execution, []byte, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	// resume_at and wait_mode stay until the first checkpoint, so the wait can
	// be restored if the instance stops before that (ReturnToWaiting).
	var state []byte
	e, err := scanExecution(s.pool.QueryRow(ctx, `UPDATE executions SET status='running', owner=$3
		WHERE id=$1 AND status='waiting' AND ($2 = '' OR wait_mode = $2)
		AND ($4 = '' OR state->>'waitNode' = $4)
		RETURNING `+execCols+`, state`, uid, mode, nullUUID(owner), nodeID), true, &state)
	if err != nil {
		return nil, nil, err
	}
	e.Owner = owner
	return e, state, nil
}

// DueWaiting lists waiting executions whose resume time has passed.
func (s *Store) DueWaiting(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM executions
		WHERE status='waiting' AND resume_at <= $1 ORDER BY resume_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id.String())
	}
	return out, rows.Err()
}

// CancelWaiting fails a waiting execution.
func (s *Store) CancelWaiting(ctx context.Context, id, by string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status='error', error=$2, finished_at=now(),
		state=NULL, resume_at=NULL, wait_mode='' WHERE id=$1 AND status='waiting'`, id, "cancelled by "+by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

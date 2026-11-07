package store

import (
	"context"
	"encoding/json"
	"errors"
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
}

func (s *Store) CreateExecution(ctx context.Context, e *Execution) error {
	e.ID = uuid.NewString()
	if e.Status == "" {
		e.Status = StatusQueued
	}
	return s.pool.QueryRow(ctx, `INSERT INTO executions
		(id, workflow_id, workflow_version, trigger_type, trigger_node, triggered_by, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING started_at`,
		e.ID, e.WorkflowID, e.WorkflowVersion, e.TriggerType, e.TriggerNode, e.TriggeredBy, e.Status,
	).Scan(&e.StartedAt)
}

func (s *Store) UpdateExecution(ctx context.Context, e *Execution) error {
	steps, err := json.Marshal(e.Steps)
	if err != nil {
		return err
	}
	// Leaving the waiting state drops the snapshot.
	_, err = s.pool.Exec(ctx, `UPDATE executions SET status=$2, error=$3, steps=$4, finished_at=$5,
		state=NULL, resume_at=NULL, wait_mode='' WHERE id=$1`,
		e.ID, e.Status, e.Error, steps, e.FinishedAt)
	return err
}

const execCols = `id, workflow_id, workflow_version, trigger_type, trigger_node, triggered_by,
	status, error, steps, resume_at, wait_mode, started_at, finished_at`

func scanExecution(row pgx.Row, withSteps bool) (*Execution, error) {
	var e Execution
	var id, wid uuid.UUID
	var steps []byte
	err := row.Scan(&id, &wid, &e.WorkflowVersion, &e.TriggerType, &e.TriggerNode, &e.TriggeredBy,
		&e.Status, &e.Error, &steps, &e.ResumeAt, &e.WaitMode, &e.StartedAt, &e.FinishedAt)
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

// FailInterrupted marks executions left queued/running by a previous process as failed.
// Assumes a single glue2flow instance; see README for scaling notes.
func (s *Store) FailInterrupted(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE executions SET status='error',
		error='interrupted: server restarted', finished_at=now()
		WHERE status IN ('queued','running')`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PauseExecution stores a waiting execution with its engine snapshot.
func (s *Store) PauseExecution(ctx context.Context, e *Execution, state []byte) error {
	steps, err := json.Marshal(e.Steps)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE executions SET status='waiting', steps=$2, state=$3,
		resume_at=$4, wait_mode=$5 WHERE id=$1`, e.ID, steps, state, e.ResumeAt, e.WaitMode)
	return err
}

// ClaimWaiting atomically moves a waiting execution to running and returns it
// with its snapshot. With mode set, only executions waiting in that mode match.
// Returns ErrNotFound if the execution is not (or no longer) waiting.
func (s *Store) ClaimWaiting(ctx context.Context, id, mode string) (*Execution, []byte, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	var state []byte
	var e Execution
	var eid, wid uuid.UUID
	var steps []byte
	err = s.pool.QueryRow(ctx, `UPDATE executions SET status='running', resume_at=NULL
		WHERE id=$1 AND status='waiting' AND ($2 = '' OR wait_mode = $2)
		RETURNING `+execCols+`, state`, uid, mode).Scan(&eid, &wid, &e.WorkflowVersion, &e.TriggerType,
		&e.TriggerNode, &e.TriggeredBy, &e.Status, &e.Error, &steps, &e.ResumeAt, &e.WaitMode,
		&e.StartedAt, &e.FinishedAt, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	e.ID, e.WorkflowID = eid.String(), wid.String()
	if err := json.Unmarshal(steps, &e.Steps); err != nil {
		return nil, nil, err
	}
	return &e, state, nil
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

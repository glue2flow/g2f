package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
)

// WaitStore persists paused executions. *store.Store implements it.
type WaitStore interface {
	PauseExecution(ctx context.Context, e *store.Execution, state []byte) error
	ClaimWaiting(ctx context.Context, id, mode string) (*store.Execution, []byte, error)
	DueWaiting(ctx context.Context, now time.Time, limit int) ([]string, error)
}

// ErrNotWaiting is returned when resuming an execution that is not waiting
// (already resumed, finished, cancelled, or waiting in another mode).
var ErrNotWaiting = errors.New("execution is not waiting")

// pause persists the run at node n. The execution's runtime limit applies per
// active segment; time spent waiting does not count.
func (e *Engine) pause(exec *store.Execution, st *runState, n *workflow.Node, step store.Step, wait *nodes.WaitRequest) *Result {
	ws, ok := e.store.(WaitStore)
	if !ok {
		return e.finish(exec, st, nil, fmt.Errorf("node %q: waiting is not supported", n.Name))
	}
	if st.Depth > 0 {
		return e.finish(exec, st, nil, fmt.Errorf("node %q: Wait cannot be used in a workflow called by another workflow", n.Name))
	}
	st.WaitNode = n.ID
	step.Status = "waiting"
	exec.Steps = append(exec.Steps, step)
	exec.Status = store.StatusWaiting
	exec.WaitMode = wait.Mode
	exec.ResumeAt = nil
	if !wait.Until.IsZero() {
		t := wait.Until.UTC()
		exec.ResumeAt = &t
	}
	data, err := json.Marshal(st)
	if err != nil {
		return e.finish(exec, st, nil, fmt.Errorf("save state: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.PauseExecution(ctx, exec, data); err != nil {
		return e.finish(exec, st, nil, fmt.Errorf("save state: %w", err))
	}
	return &Result{Execution: exec}
}

// completeWaitStep turns the "waiting" step of nodeID into a successful one.
func (e *Engine) completeWaitStep(exec *store.Execution, nodeID string, out nodes.Output) {
	for i := len(exec.Steps) - 1; i >= 0; i-- {
		s := &exec.Steps[i]
		if s.NodeID == nodeID && s.Status == "waiting" {
			s.Status = "success"
			s.DurationMs = time.Since(s.StartedAt).Milliseconds()
			s.Output, s.Truncated = e.truncate(out)
			return
		}
	}
}

// Resume continues a waiting execution in the background.
//
// With item set (the resume URL was called) the Wait node emits that item on
// "main". Otherwise the resume time has come: webhook waits emit their input on
// "timeout", time-based waits pass their input on "main".
func (e *Engine) Resume(ctx context.Context, execID string, item nodes.Item) (*store.Execution, error) {
	ws, ok := e.store.(WaitStore)
	if !ok {
		return nil, errors.New("waiting is not supported")
	}
	mode := ""
	if item != nil {
		mode = nodes.WaitWebhook
	}
	exec, data, err := ws.ClaimWaiting(ctx, execID, mode)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotWaiting
	}
	if err != nil {
		return nil, err
	}
	var st runState
	if err := json.Unmarshal(data, &st); err != nil {
		e.finish(exec, nil, nil, fmt.Errorf("corrupt execution state: %w", err))
		return nil, err
	}
	in := concatInputs(st.Inputs[st.WaitNode])
	if in == nil {
		in = []nodes.Item{}
	}
	var out nodes.Output
	switch {
	case item != nil:
		out = nodes.Output{"main": {item}, "timeout": {}}
	case exec.WaitMode == nodes.WaitWebhook:
		out = nodes.Output{"main": {}, "timeout": in}
	default:
		out = nodes.Output{"main": in, "timeout": {}}
	}
	exec.WaitMode = ""
	e.background(exec, &st, out)
	return exec, nil
}

// RunResumer resumes executions whose wait time has passed, polling every
// interval. Safe to run on every replica: resuming claims the execution.
func (e *Engine) RunResumer(ctx context.Context, every time.Duration) {
	ws, ok := e.store.(WaitStore)
	if !ok {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ids, err := ws.DueWaiting(ctx, time.Now(), 100)
		if err != nil {
			e.log.Error("load due executions", "err", err)
			continue
		}
		for _, id := range ids {
			if _, err := e.Resume(ctx, id, nil); err != nil && !errors.Is(err, ErrNotWaiting) {
				e.log.Error("resume execution", "execution", id, "err", err)
			}
		}
	}
}

package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
)

// MaxDepth limits sub-workflow nesting (and accidental recursion).
const MaxDepth = 5

// runtime implements nodes.Runtime for one run.
type runtime struct {
	e      *Engine
	st     *runState
	execID string
}

func (r *runtime) ResumeURL() string {
	if r.e.opts.ResumeURL == nil {
		return ""
	}
	return r.e.opts.ResumeURL(r.execID)
}

// RunWorkflow runs a workflow of the same organization from its "Called by
// Workflow" trigger, synchronously, as its own execution.
func (r *runtime) RunWorkflow(ctx context.Context, workflowID string, items []nodes.Item) ([]nodes.Item, error) {
	e := r.e
	if e.opts.Workflows == nil {
		return nil, errors.New("sub-workflows are not available")
	}
	if r.st.Depth+1 > MaxDepth {
		return nil, fmt.Errorf("sub-workflows nested deeper than %d levels", MaxDepth)
	}
	wf, err := e.opts.Workflows.GetWorkflow(ctx, workflowID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && wf.OrgID != r.st.Workflow.OrgID) {
		return nil, fmt.Errorf("workflow %s not found", workflowID)
	}
	if err != nil {
		return nil, err
	}
	req := Request{
		Workflow: wf, TriggerType: nodes.TriggerCall, Items: items,
		TriggeredBy: "workflow: " + r.st.Workflow.Name,
	}
	exec, start, err := e.prepare(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("workflow %q: %w", wf.Name, err)
	}
	// Nested runs share the caller's worker slot (no semaphore) to avoid
	// deadlocks when all workers wait for children.
	res := e.execute(ctx, exec, newState(req, start, r.st.Depth+1), nil)
	if res.Execution.Status != store.StatusSuccess {
		return nil, fmt.Errorf("workflow %q failed: %s", wf.Name, res.Execution.Error)
	}
	if res.LastOutput == nil {
		return []nodes.Item{}, nil
	}
	return res.LastOutput, nil
}

package trigger

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// ErrorStore finds workflows with error triggers.
type ErrorStore interface {
	WorkflowsWithNodeType(ctx context.Context, org, nodeType string) ([]*workflow.Workflow, error)
}

// ErrorDispatcher starts Error Trigger workflows when an execution fails.
// It is installed as engine.Options.OnFailure.
type ErrorDispatcher struct {
	store     ErrorStore
	eng       Engine
	reg       *nodes.Registry
	log       *slog.Logger
	publicURL string
}

func NewErrorDispatcher(st ErrorStore, eng Engine, reg *nodes.Registry, log *slog.Logger, publicURL string) *ErrorDispatcher {
	return &ErrorDispatcher{store: st, eng: eng, reg: reg, log: log.With("component", "errors"), publicURL: strings.TrimRight(publicURL, "/")}
}

// SetEngine breaks the construction cycle (the engine needs the dispatcher as
// its failure hook, the dispatcher needs the engine to start runs).
func (d *ErrorDispatcher) SetEngine(eng Engine) { d.eng = eng }

// WorkflowFailed fires matching error triggers of the failed workflow's organization.
func (d *ErrorDispatcher) WorkflowFailed(exec *store.Execution, failed *workflow.Workflow) {
	if exec.TriggerType == string(nodes.TriggerError) || d.eng == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	candidates, err := d.store.WorkflowsWithNodeType(ctx, failed.OrgID, "trigger.error")
	if err != nil {
		d.log.Error("find error workflows", "err", err)
		return
	}
	item := nodes.Item{
		"workflow": map[string]any{"id": failed.ID, "name": failed.Name, "url": d.publicURL + "/#/w/" + failed.ID},
		"execution": map[string]any{
			"id": exec.ID, "triggerType": exec.TriggerType, "triggeredBy": exec.TriggeredBy,
			"startedAt": exec.StartedAt.UTC().Format(time.RFC3339),
		},
		"error":      exec.Error,
		"failedNode": failedNode(exec),
	}
	for _, wf := range candidates {
		for _, n := range wf.Graph.Nodes {
			if n.Type != "trigger.error" || n.Disabled || !nodes.ErrorTriggerWatches(n.Params, wf.ID, failed.ID, exec.TriggerType) {
				continue
			}
			// Watching other workflows requires an active error workflow; an
			// error handler inside the failed workflow itself always runs.
			if wf.ID != failed.ID && !wf.Active {
				continue
			}
			started, err := d.eng.Start(ctx, engine.Request{
				Workflow: wf, TriggerType: nodes.TriggerError, TriggerNode: n.ID,
				TriggeredBy: "error in " + failed.Name, Items: []nodes.Item{item},
			})
			if err != nil {
				d.log.Error("start error workflow", "workflow", wf.ID, "err", err)
				continue
			}
			d.log.Info("error trigger fired", "workflow", wf.ID, "failed", failed.ID, "execution", started.ID)
		}
	}
}

func failedNode(exec *store.Execution) string {
	for i := len(exec.Steps) - 1; i >= 0; i-- {
		if exec.Steps[i].Status == "error" {
			return exec.Steps[i].NodeName
		}
	}
	return ""
}

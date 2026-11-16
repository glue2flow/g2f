package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// Poll runs one check of a polling trigger node, outside of an execution, and
// returns the new items it found. The scheduler starts a run only when there
// are any, so idle checks leave no executions behind.
//
// A failing check is recorded as a failed execution (visible in the editor,
// notifies error workflows) when its error differs from the previous check's.
// The last error is kept in the workflow state, so this holds across replicas.
func (e *Engine) Poll(ctx context.Context, wf *workflow.Workflow, nodeID string) ([]nodes.Item, error) {
	items, err := e.poll(ctx, wf, nodeID)
	state := e.stateFor(wf.ID)
	const scope, key = "pollstatus", "lastError"
	if err == nil {
		if state != nil {
			if _, ok, _ := state.Get(ctx, scope+":"+nodeID, key); ok {
				_, _ = state.Delete(ctx, scope+":"+nodeID, key)
			}
		}
		return items, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	record := true
	if state != nil {
		last, _, _ := state.Get(ctx, scope+":"+nodeID, key)
		record = last != err.Error()
		_ = state.Set(ctx, scope+":"+nodeID, key, err.Error(), 0)
	}
	if record {
		e.RecordFailure(context.WithoutCancel(ctx), wf, nodeID, nodes.TriggerPoll, err)
	}
	return nil, err
}

func (e *Engine) poll(ctx context.Context, wf *workflow.Workflow, nodeID string) ([]nodes.Item, error) {
	n := wf.Graph.Node(nodeID)
	if n == nil {
		return nil, fmt.Errorf("node %s not found", nodeID)
	}
	if e.reg.TriggerKind(n.Type) != nodes.TriggerPoll {
		return nil, fmt.Errorf("node %q is not a polling trigger", n.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()

	var secrets map[string]map[string]string
	if e.opts.Secrets != nil {
		var err error
		if secrets, err = e.opts.Secrets.NodeSecrets(ctx, wf.ID); err != nil {
			return nil, fmt.Errorf("load secrets: %w", err)
		}
	}
	st := &runState{Workflow: wfMeta{ID: wf.ID, Name: wf.Name, OrgID: wf.OrgID, Version: wf.Version}}
	log := e.log.With("workflow", wf.ID, "node", n.Name, "poll", true)
	rc := &runCtx{
		settings: map[string]moduleSettings{}, scrub: newScrubber(secrets), log: log,
		triggerType: string(nodes.TriggerPoll), state: e.stateFor(wf.ID),
	}
	env := expression.Env{
		"workflow":  map[string]any{"id": wf.ID, "name": wf.Name},
		"execution": map[string]any{"triggerType": string(nodes.TriggerPoll)},
		"nodes":     map[string]any{},
		"trigger":   nodes.Item{},
	}
	var logs []store.LogEntry
	out, err := e.runNode(ctx, st, withSecrets(n, secrets[n.ID]), nil, nil, env, rc, &logs)
	for _, l := range logs {
		log.Info("poll log", "level", l.Level, "message", l.Message)
	}
	if err != nil {
		var wait *nodes.WaitRequest
		if errors.As(err, &wait) {
			return nil, errors.New("polling triggers cannot wait")
		}
		return nil, errors.New(rc.scrub.clean(err.Error()))
	}
	return flatten(out), nil
}

// RecordFailure stores a failed execution for a trigger that could not start
// a run (e.g. a polling trigger whose mail server rejects the login), so the
// problem shows up in the editor and error workflows are notified.
func (e *Engine) RecordFailure(ctx context.Context, wf *workflow.Workflow, nodeID string, kind nodes.TriggerKind, cause error) {
	req := Request{Workflow: wf, TriggerType: kind, TriggerNode: nodeID, TriggeredBy: string(kind)}
	exec, start, err := e.prepare(ctx, req)
	if err != nil {
		e.log.Error("record trigger failure", "workflow", wf.ID, "err", err)
		return
	}
	st := newState(req, start, 0)
	exec.Steps = append(exec.Steps, store.Step{
		NodeID: start.ID, NodeName: start.Name, NodeType: start.Type, StartedAt: exec.StartedAt,
		Status: "error", Error: cause.Error(),
	})
	e.finish(exec, st, nil, fmt.Errorf("node %q: %s", start.Name, cause))
}

// TriggerContext builds the context a check of a polling trigger gets
// (params with secrets, module settings, the selected account or
// credential), for work on the trigger outside a check: push listeners and
// push subscriptions resolve credentials exactly like the check does.
func (e *Engine) TriggerContext(ctx context.Context, wf *workflow.Workflow, nodeID string) (*nodes.ExecContext, error) {
	n := wf.Graph.Node(nodeID)
	if n == nil {
		return nil, fmt.Errorf("node %s not found", nodeID)
	}
	impl, ok := e.reg.Get(n.Type)
	if !ok {
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	def := impl.Definition()
	var secrets map[string]map[string]string
	if e.opts.Secrets != nil {
		var err error
		if secrets, err = e.opts.Secrets.NodeSecrets(ctx, wf.ID); err != nil {
			return nil, fmt.Errorf("load secrets: %w", err)
		}
	}
	log := e.log.With("workflow", wf.ID, "node", n.Name)
	rc := &runCtx{settings: map[string]moduleSettings{}, scrub: newScrubber(secrets), log: log, state: e.stateFor(wf.ID)}
	n = withSecrets(n, secrets[n.ID])
	ms, token, credential, err := e.resolveNode(ctx, wf.OrgID, n, &def, rc)
	if err != nil {
		return nil, errors.New(rc.scrub.clean(err.Error()))
	}
	env := expression.Env{
		"workflow":  map[string]any{"id": wf.ID, "name": wf.Name},
		"execution": map[string]any{"triggerType": string(nodes.TriggerPoll)},
		"nodes":     map[string]any{},
		"trigger":   nodes.Item{},
	}
	return &nodes.ExecContext{
		Node: n, Def: &def, Items: []nodes.Item{}, Config: ms.config, Vars: ms.vars, OAuth: token, Credential: credential,
		Env: expression.WithContext(env, ctx), Logger: log, State: rc.state, TriggerType: string(nodes.TriggerPoll),
	}, nil
}

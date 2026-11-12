// Package engine executes workflow graphs.
//
// Execution model: a run starts at one trigger node. All nodes reachable from
// it are executed once, in topological order. A node's input is the
// concatenation of all items arriving on its incoming edges; a node that
// receives no items (e.g. the unused branch of a Condition) is skipped, and so are
// its descendants unless they receive items from elsewhere.
//
// The whole progress of a run lives in a serializable runState, so a run can
// pause at a Wait node, be persisted, and continue later (see wait.go).
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// SecretSource provides decrypted node secrets and plugin configuration.
type SecretSource interface {
	NodeSecrets(ctx context.Context, workflowID string) (map[string]map[string]string, error)
	// ModuleSettings returns the plugin-only config and the editor-visible vars.
	ModuleSettings(ctx context.Context, org, module string) (config, vars map[string]any, err error)
}

// ExecStore is the persistence the engine needs. Stores that also implement
// WaitStore enable the Wait node.
type ExecStore interface {
	CreateExecution(ctx context.Context, e *store.Execution) error
	UpdateExecution(ctx context.Context, e *store.Execution) error
}

// TokenSource returns fresh OAuth2 access tokens of connected accounts.
type TokenSource interface {
	Token(ctx context.Context, org, module, connectionID string) (*nodes.OAuthToken, error)
}

// WorkflowLoader loads workflows for sub-workflow calls.
type WorkflowLoader interface {
	GetWorkflow(ctx context.Context, id string) (*workflow.Workflow, error)
}

type Options struct {
	// Secrets is optional; without it nodes see no secrets and no plugin config.
	Secrets SecretSource
	// Tokens is optional; without it nodes of OAuth2 plugins fail.
	Tokens TokenSource
	// Workflows is optional; without it Execute Workflow nodes fail.
	Workflows WorkflowLoader
	// ResumeURL builds the signed URL that resumes the given Wait node of an
	// execution.
	ResumeURL func(executionID, nodeID string) string
	// State is optional; without it the Data Store, Remove Duplicates (across
	// runs) and polling triggers fail.
	State StateBackend
	// OnFailure is called (in the background) after an execution failed.
	OnFailure func(exec *store.Execution, wf *workflow.Workflow)

	Workers        int
	Timeout        time.Duration
	MaxStoredItems int
}

type Engine struct {
	reg   *nodes.Registry
	store ExecStore
	log   *slog.Logger
	opts  Options

	sem     chan struct{}
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc

	// instance identifies this engine; rs is set when the store supports
	// checkpoints (see recovery.go).
	instance string
	rs       RecoveryStore
	draining atomic.Bool
}

func New(reg *nodes.Registry, st ExecStore, log *slog.Logger, opts Options) *Engine {
	if opts.Workers <= 0 {
		opts.Workers = 16
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.MaxStoredItems <= 0 {
		opts.MaxStoredItems = 100
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		reg: reg, store: st, log: log, opts: opts,
		sem: make(chan struct{}, opts.Workers), baseCtx: ctx, cancel: cancel,
		instance: uuid.NewString(),
	}
	e.rs, _ = st.(RecoveryStore)
	return e
}

// Request describes one workflow run.
type Request struct {
	Workflow    *workflow.Workflow
	TriggerType nodes.TriggerKind
	// TriggerNode is the id of the trigger node to start from. If empty, the
	// first trigger node of TriggerType is used.
	TriggerNode string
	// Items is the trigger payload (e.g. the webhook request). If empty the
	// trigger node produces its own default items.
	Items       []nodes.Item
	TriggeredBy string
}

// Result is returned by synchronous runs.
type Result struct {
	Execution *store.Execution
	// LastOutput is the output of the last node that ran.
	LastOutput []nodes.Item
}

// runState is everything needed to continue a run. It is persisted as JSON
// while the execution waits, so it must only contain JSON-friendly data.
type runState struct {
	Workflow wfMeta                             `json:"workflow"`
	Graph    workflow.Graph                     `json:"graph"` // the version the run started with
	StartID  string                             `json:"startId"`
	Depth    int                                `json:"depth"` // sub-workflow nesting level
	Done     map[string]bool                    `json:"done"`
	Received map[string]bool                    `json:"received"`
	Inputs   map[string]map[string][]nodes.Item `json:"inputs"`  // node -> input handle -> items
	Outputs  map[string]any                     `json:"outputs"` // node name -> items (expression scope)
	Trigger  nodes.Item                         `json:"trigger"`
	Last     []nodes.Item                       `json:"last"`
	WaitNode string                             `json:"waitNode,omitempty"`

	// recovered marks a run continued after its instance stopped (not persisted).
	recovered bool
}

type wfMeta struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OrgID   string `json:"orgId"`
	Version int    `json:"version"`
}

func (m wfMeta) workflow() *workflow.Workflow {
	return &workflow.Workflow{ID: m.ID, Name: m.Name, OrgID: m.OrgID, Version: m.Version}
}

func newState(req Request, start *workflow.Node, depth int) *runState {
	w := req.Workflow
	return &runState{
		Workflow: wfMeta{ID: w.ID, Name: w.Name, OrgID: w.OrgID, Version: w.Version},
		Graph:    w.Graph, StartID: start.ID, Depth: depth,
		Done:     map[string]bool{},
		Received: map[string]bool{start.ID: true},
		Inputs:   map[string]map[string][]nodes.Item{start.ID: {workflow.DefaultHandle: req.Items}},
		Outputs:  map[string]any{},
		Trigger:  nodes.Item{},
	}
}

// Start persists a queued execution and runs it in the background.
func (e *Engine) Start(ctx context.Context, req Request) (*store.Execution, error) {
	exec, start, err := e.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	st := newState(req, start, 0)
	// Save the trigger items, so another instance can run it if this one stops.
	if err := e.checkpoint(exec, st); err != nil {
		return nil, err
	}
	e.background(exec, st, nil)
	return exec, nil
}

// background runs exec in a goroutine, bounded by the worker pool. exec is
// copied first because the goroutine keeps mutating its own copy.
func (e *Engine) background(exec *store.Execution, st *runState, resume nodes.Output) {
	own := *exec
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		select {
		case e.sem <- struct{}{}:
		case <-e.baseCtx.Done():
			if e.rs != nil && st.Depth == 0 {
				e.release(&own, st)
			} else {
				e.finish(&own, st, nil, errors.New("server shutting down"))
			}
			return
		}
		defer func() { <-e.sem }()
		e.execute(e.baseCtx, &own, st, resume)
	}()
}

// Run executes synchronously and returns the result, bounded by ctx.
func (e *Engine) Run(ctx context.Context, req Request) (*Result, error) {
	exec, start, err := e.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	st := newState(req, start, 0)
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return e.finish(exec, st, nil, ctx.Err()), nil
	}
	defer func() { <-e.sem }()
	e.wg.Add(1)
	defer e.wg.Done()
	return e.execute(ctx, exec, st, nil), nil
}

// Shutdown lets running executions finish for up to grace, then cancels the
// rest and waits up to timeout for them to record their state. With a
// RecoveryStore, cancelled executions are handed to another instance instead
// of failing. Waiting executions are unaffected.
func (e *Engine) Shutdown(grace, timeout time.Duration) {
	e.draining.Store(true)
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		e.log.Info("handing over unfinished executions")
		e.cancel()
		select {
		case <-done:
		case <-time.After(timeout):
			e.log.Warn("engine shutdown timed out")
		}
	}
	e.cancel()
	if e.rs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.rs.RemoveInstance(ctx, e.instance); err != nil {
			e.log.Error("unregister instance", "err", err)
		}
	}
}

func (e *Engine) prepare(ctx context.Context, req Request) (*store.Execution, *workflow.Node, error) {
	start, err := e.findTrigger(req)
	if err != nil {
		return nil, nil, err
	}
	exec := &store.Execution{
		WorkflowID:      req.Workflow.ID,
		WorkflowVersion: req.Workflow.Version,
		TriggerType:     string(req.TriggerType),
		TriggerNode:     start.ID,
		TriggeredBy:     req.TriggeredBy,
		Status:          store.StatusQueued,
	}
	if e.rs != nil {
		exec.Owner = e.instance
	}
	if err := e.store.CreateExecution(ctx, exec); err != nil {
		return nil, nil, fmt.Errorf("create execution: %w", err)
	}
	return exec, start, nil
}

func (e *Engine) findTrigger(req Request) (*workflow.Node, error) {
	g := &req.Workflow.Graph
	if req.TriggerNode != "" {
		n := g.Node(req.TriggerNode)
		if n == nil {
			return nil, fmt.Errorf("trigger node %q not found", req.TriggerNode)
		}
		kind := e.reg.TriggerKind(n.Type)
		if kind == "" {
			return nil, fmt.Errorf("node %q is not a trigger", n.Name)
		}
		// Manual runs may start from any trigger (useful to test webhook/schedule flows).
		if req.TriggerType != nodes.TriggerManual && kind != req.TriggerType {
			return nil, fmt.Errorf("node %q is a %s trigger, not %s", n.Name, kind, req.TriggerType)
		}
		if n.Disabled {
			return nil, fmt.Errorf("trigger node %q is disabled", n.Name)
		}
		return n, nil
	}
	var fallback *workflow.Node
	for i := range g.Nodes {
		n := &g.Nodes[i]
		kind := e.reg.TriggerKind(n.Type)
		if kind == "" || n.Disabled {
			continue
		}
		if kind == req.TriggerType {
			return n, nil
		}
		if fallback == nil {
			fallback = n
		}
	}
	if req.TriggerType == nodes.TriggerManual && fallback != nil {
		return fallback, nil
	}
	if req.TriggerType == nodes.TriggerCall {
		return nil, errors.New("workflow has no 'Called by Workflow' trigger")
	}
	return nil, fmt.Errorf("workflow has no %s trigger", req.TriggerType)
}

// execute runs (or, with resume set, continues) a run until it finishes or
// pauses. resume is the output of the Wait node the run paused at.
func (e *Engine) execute(ctx context.Context, exec *store.Execution, st *runState, resume nodes.Output) *Result {
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()

	exec.Status = store.StatusRunning
	switch {
	case resume != nil:
		// Claiming the wait already marked it running; the first checkpoint
		// follows once the Wait node's output is processed.
	case e.rs != nil && exec.Owner != "":
		if err := e.checkpoint(exec, st); err != nil {
			return e.abandon(exec, err)
		}
	default:
		if err := e.store.UpdateExecution(ctx, exec); err != nil {
			e.log.Error("update execution", "id", exec.ID, "err", err)
		}
	}

	log := e.log.With("workflow", st.Workflow.ID, "execution", exec.ID)
	g := &st.Graph
	reachable := g.Reachable(st.StartID)
	order, err := g.TopoOrder(reachable)
	if err != nil {
		return e.finish(exec, st, nil, err)
	}

	env := e.newEnv(st, exec)

	var secrets map[string]map[string]string
	if e.opts.Secrets != nil {
		if secrets, err = e.opts.Secrets.NodeSecrets(ctx, st.Workflow.ID); err != nil {
			return e.finish(exec, st, nil, fmt.Errorf("load secrets: %w", err))
		}
	}
	rc := &runCtx{
		settings: map[string]moduleSettings{}, scrub: newScrubber(secrets),
		rt: &runtime{e: e, st: st}, log: log,
		triggerType: exec.TriggerType, state: e.stateFor(st.Workflow.ID),
	}

	for _, id := range order {
		if st.Done[id] {
			continue
		}
		n := g.Node(id)
		var out nodes.Output

		if resume != nil && id == st.WaitNode {
			// Continue after the Wait node: its output decides the branch.
			out = resume
			resume = nil
			st.WaitNode = ""
			e.completeWaitStep(exec, id, out)
		} else {
			step := store.Step{NodeID: n.ID, NodeName: n.Name, NodeType: n.Type, StartedAt: time.Now().UTC()}
			byHandle := st.Inputs[id]
			in := concatInputs(byHandle)
			step.InputCount = len(in)

			if !st.Received[id] || (id != st.StartID && len(in) == 0) {
				step.Status = "skipped"
				exec.Steps = append(exec.Steps, step)
				st.Done[id] = true
				continue
			}

			if st.recovered {
				// The first node that runs after a takeover may have run before.
				st.recovered = false
				step.Logs = append(step.Logs, store.LogEntry{Time: time.Now().UTC(), Level: nodes.LogWarn,
					Message: fmt.Sprintf("The instance running this execution stopped; continued on another one (attempt %d). "+
						"This node may have run before.", exec.Attempts+1)})
			}
			if n.Disabled {
				out = nodes.Output{workflow.DefaultHandle: in}
			} else {
				out, err = e.runNode(ctx, st, withSecrets(n, secrets[n.ID]), byHandle, in, env, rc, &step.Logs)
			}
			step.DurationMs = time.Since(step.StartedAt).Milliseconds()

			var wait *nodes.WaitRequest
			if errors.As(err, &wait) {
				return e.pause(exec, st, n, step, wait)
			}
			if err != nil && e.shouldRelease(ctx, st) {
				// Shutting down: another instance runs this node again.
				return e.release(exec, st)
			}
			if err != nil {
				// Error messages are stored and shown to viewers: never include secrets.
				msg := rc.scrub.clean(err.Error())
				step.Status = "error"
				step.Error = msg
				exec.Steps = append(exec.Steps, step)
				return e.finish(exec, st, nil, fmt.Errorf("node %q: %s", n.Name, msg))
			}
			step.Status = "success"
			step.Output, step.Truncated = e.truncate(out)
			exec.Steps = append(exec.Steps, step)
		}

		st.Done[id] = true
		all := flatten(out)
		st.Outputs[n.Name] = all
		if id == st.StartID && len(all) > 0 {
			st.Trigger = all[0]
			env["trigger"] = st.Trigger
		}
		st.Last = all

		for _, edge := range g.Edges {
			if edge.Source != id || !reachable[edge.Target] {
				continue
			}
			st.Received[edge.Target] = true
			th := e.targetHandle(g, edge)
			if st.Inputs[edge.Target] == nil {
				st.Inputs[edge.Target] = map[string][]nodes.Item{}
			}
			st.Inputs[edge.Target][th] = append(st.Inputs[edge.Target][th], out[edge.Handle()]...)
		}
		if err := e.checkpoint(exec, st); err != nil {
			return e.abandon(exec, err)
		}
	}
	return e.finish(exec, st, st.Last, nil)
}

// abandon stops a run that another instance took over, without writing it.
func (e *Engine) abandon(exec *store.Execution, err error) *Result {
	e.log.Warn("stopping execution", "id", exec.ID, "reason", err)
	return &Result{Execution: exec}
}

// runCtx holds per-run helpers that are not persisted.
type runCtx struct {
	settings    map[string]moduleSettings
	scrub       *scrubber
	rt          *runtime
	log         *slog.Logger
	triggerType string
	state       nodes.StateStore
}

func (e *Engine) stateFor(workflowID string) nodes.StateStore {
	if e.opts.State == nil {
		return nil
	}
	return workflowState{b: e.opts.State, id: workflowID}
}

// targetHandle returns the input handle an edge feeds; edges without one go to
// the target's first input.
func (e *Engine) targetHandle(g *workflow.Graph, edge workflow.Edge) string {
	if edge.TargetHandle != "" {
		return edge.TargetHandle
	}
	if t := g.Node(edge.Target); t != nil {
		if impl, ok := e.reg.Get(t.Type); ok {
			if ins := impl.Definition().Inputs; len(ins) > 0 {
				return ins[0]
			}
		}
	}
	return workflow.DefaultHandle
}

func concatInputs(byHandle map[string][]nodes.Item) []nodes.Item {
	if len(byHandle) == 1 {
		for _, items := range byHandle {
			return items
		}
	}
	keys := make([]string, 0, len(byHandle))
	for k := range byHandle {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var all []nodes.Item
	for _, k := range keys {
		all = append(all, byHandle[k]...)
	}
	return all
}

// withSecrets returns a copy of n whose params include its decrypted secrets.
func withSecrets(n *workflow.Node, secrets map[string]string) *workflow.Node {
	if len(secrets) == 0 {
		return n
	}
	c := *n
	c.Params = make(map[string]any, len(n.Params)+len(secrets))
	maps.Copy(c.Params, n.Params)
	for k, v := range secrets {
		c.Params[k] = v
	}
	return &c
}

func (e *Engine) runNode(ctx context.Context, st *runState, n *workflow.Node, byHandle map[string][]nodes.Item,
	in []nodes.Item, env expression.Env, rc *runCtx, logs *[]store.LogEntry) (out nodes.Output, err error) {
	impl, ok := e.reg.Get(n.Type)
	if !ok {
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	def := impl.Definition()
	var ms moduleSettings
	if mod := def.Module; mod != "" && mod != nodes.BuiltinModule && e.opts.Secrets != nil {
		if ms, ok = rc.settings[mod]; !ok {
			if ms.config, ms.vars, err = e.opts.Secrets.ModuleSettings(ctx, st.Workflow.OrgID, mod); err != nil {
				return nil, fmt.Errorf("load %s settings: %w", mod, err)
			}
			rc.settings[mod] = ms
			if info, ok := e.reg.Module(mod); ok {
				for _, p := range info.Config {
					if s, isStr := ms.config[p.Name].(string); isStr && p.Type == nodes.ParamSecret {
						rc.scrub.add(s)
					}
				}
			}
		}
	}
	var token *nodes.OAuthToken
	if def.Param(nodes.ConnectionParam) != nil {
		id, _ := n.Params[nodes.ConnectionParam].(string)
		if id == "" {
			return nil, errors.New("no account selected")
		}
		if e.opts.Tokens == nil {
			return nil, errors.New("connected accounts are not available")
		}
		if token, err = e.opts.Tokens.Token(ctx, st.Workflow.OrgID, def.Module, id); err != nil {
			return nil, fmt.Errorf("account: %w", err)
		}
		rc.scrub.add(token.AccessToken)
	}
	defer func() {
		if r := recover(); r != nil {
			rc.log.Error("node panic", "node", n.Name, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		in = []nodes.Item{}
	}
	return impl.Execute(ctx, &nodes.ExecContext{
		Node: n, Def: &def, Items: in, Inputs: byHandle, Config: ms.config, Vars: ms.vars, OAuth: token,
		Env: env, Logger: rc.log, Runtime: rc.rt, LogSink: rc.logSink(logs),
		State: rc.state, TriggerType: rc.triggerType,
	})
}

func (e *Engine) finish(exec *store.Execution, st *runState, last []nodes.Item, err error) *Result {
	now := time.Now().UTC()
	exec.FinishedAt = &now
	exec.ResumeAt = nil
	if err != nil {
		exec.Status = store.StatusError
		exec.Error = err.Error()
	} else {
		exec.Status = store.StatusSuccess
	}
	// Use a fresh context so results are stored even after cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if uerr := e.store.UpdateExecution(ctx, exec); errors.Is(uerr, store.ErrNotOwner) {
		e.log.Warn("execution was taken over by another instance; result discarded", "id", exec.ID)
		return &Result{Execution: exec, LastOutput: last}
	} else if uerr != nil {
		e.log.Error("store execution result", "id", exec.ID, "err", uerr)
		// Never leave an execution "running": retry without node outputs.
		for i := range exec.Steps {
			exec.Steps[i].Output = nil
		}
		if exec.Error == "" {
			exec.Error = "node outputs could not be stored: " + uerr.Error()
		}
		if uerr := e.store.UpdateExecution(ctx, exec); uerr != nil {
			e.log.Error("store execution result without outputs", "id", exec.ID, "err", uerr)
		}
	}
	if err != nil && e.opts.OnFailure != nil && st != nil {
		snapshot := *exec
		go e.opts.OnFailure(&snapshot, st.Workflow.workflow())
	}
	return &Result{Execution: exec, LastOutput: last}
}

func (e *Engine) truncate(out nodes.Output) (map[string][]map[string]any, bool) {
	res := make(map[string][]map[string]any, len(out))
	truncated := false
	for h, items := range out {
		if len(items) > e.opts.MaxStoredItems {
			items = items[:e.opts.MaxStoredItems]
			truncated = true
		}
		res[h] = items
	}
	return res, truncated
}

func flatten(out nodes.Output) []nodes.Item {
	if len(out) == 1 {
		for _, items := range out {
			return items
		}
	}
	var all []nodes.Item
	// "main" first, then other handles in a stable order.
	all = append(all, out[workflow.DefaultHandle]...)
	for _, h := range []string{"true", "false"} {
		all = append(all, out[h]...)
	}
	rest := make([]string, 0, len(out))
	for h := range out {
		if h != workflow.DefaultHandle && h != "true" && h != "false" {
			rest = append(rest, h)
		}
	}
	sort.Strings(rest)
	for _, h := range rest {
		all = append(all, out[h]...)
	}
	return all
}

type moduleSettings struct{ config, vars map[string]any }

// scrubber removes known secret values from messages.
// Limits for the execution log of a single step.
const (
	maxStepLogs       = 500
	maxLogMessageSize = 8 << 10
)

// logSink collects a node's log messages into the step. Messages are scrubbed
// of known secrets because executions are visible to every workflow viewer.
func (rc *runCtx) logSink(logs *[]store.LogEntry) func(level, message string) {
	return func(level, message string) {
		switch n := len(*logs); {
		case n > maxStepLogs:
			return
		case n == maxStepLogs:
			level, message = nodes.LogWarn, fmt.Sprintf("more than %d log messages, the rest were dropped", maxStepLogs)
		}
		message = rc.scrub.clean(message)
		if len(message) > maxLogMessageSize {
			message = truncateUTF8(message, maxLogMessageSize) + "…"
		}
		*logs = append(*logs, store.LogEntry{Time: time.Now().UTC(), Level: level, Message: message})
	}
}

func truncateUTF8(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type scrubber struct{ values []string }

func newScrubber(secrets map[string]map[string]string) *scrubber {
	s := &scrubber{}
	for _, params := range secrets {
		for _, v := range params {
			s.add(v)
		}
	}
	return s
}

func (s *scrubber) add(v string) {
	if len(v) >= 4 {
		s.values = append(s.values, v)
	}
}

func (s *scrubber) clean(msg string) string {
	for _, v := range s.values {
		msg = strings.ReplaceAll(msg, v, "••••••")
	}
	return msg
}

// newEnv builds the expression scope of a run. execution.resumeUrls maps the
// name of every enabled Wait node to its resume URL, so a node before the Wait
// can send it; execution.resumeUrl is set only if there is exactly one.
func (e *Engine) newEnv(st *runState, exec *store.Execution) expression.Env {
	execInfo := map[string]any{"id": exec.ID, "triggerType": exec.TriggerType}
	if e.opts.ResumeURL != nil {
		urls := map[string]any{}
		var last string
		count := 0
		for _, n := range st.Graph.Nodes {
			if n.Type == "core.wait" && !n.Disabled {
				last = e.opts.ResumeURL(exec.ID, n.ID)
				urls[n.Name] = last
				count++
			}
		}
		execInfo["resumeUrls"] = urls
		if count == 1 {
			execInfo["resumeUrl"] = last
		}
	}
	return expression.Env{
		"workflow":  map[string]any{"id": st.Workflow.ID, "name": st.Workflow.Name},
		"execution": execInfo,
		"nodes":     st.Outputs,
		"trigger":   st.Trigger,
	}
}

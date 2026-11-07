// Package engine executes workflow graphs.
//
// Execution model: a run starts at one trigger node. All nodes reachable from
// it are executed once, in topological order. A node's input is the
// concatenation of all items arriving on its incoming edges; a node that
// receives no items (e.g. the unused branch of an If) is skipped, and so are
// its descendants unless they receive items from elsewhere.
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
	"time"

	"github.com/wolfposd/g8g/internal/expression"
	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
)

// SecretSource provides decrypted node secrets and plugin configuration.
type SecretSource interface {
	NodeSecrets(ctx context.Context, workflowID string) (map[string]map[string]string, error)
	// ModuleSettings returns the plugin-only config and the editor-visible vars.
	ModuleSettings(ctx context.Context, org, module string) (config, vars map[string]any, err error)
}

// ExecStore is the persistence the engine needs.
type ExecStore interface {
	CreateExecution(ctx context.Context, e *store.Execution) error
	UpdateExecution(ctx context.Context, e *store.Execution) error
}

type Options struct {
	// Secrets is optional; without it nodes see no secrets and no plugin config.
	Secrets        SecretSource
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
	return &Engine{
		reg: reg, store: st, log: log, opts: opts,
		sem: make(chan struct{}, opts.Workers), baseCtx: ctx, cancel: cancel,
	}
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

// Start persists a queued execution and runs it in the background.
func (e *Engine) Start(ctx context.Context, req Request) (*store.Execution, error) {
	exec, start, err := e.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	// The goroutine keeps mutating exec; callers get a snapshot.
	snapshot := *exec
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		select {
		case e.sem <- struct{}{}:
		case <-e.baseCtx.Done():
			e.finish(exec, nil, errors.New("server shutting down"))
			return
		}
		defer func() { <-e.sem }()
		e.execute(e.baseCtx, exec, req, start)
	}()
	return &snapshot, nil
}

// Run executes synchronously and returns the result, bounded by ctx.
func (e *Engine) Run(ctx context.Context, req Request) (*Result, error) {
	exec, start, err := e.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		e.finish(exec, nil, ctx.Err())
		return &Result{Execution: exec}, nil
	}
	defer func() { <-e.sem }()
	e.wg.Add(1)
	defer e.wg.Done()
	return e.execute(ctx, exec, req, start), nil
}

// Shutdown cancels running executions and waits for them to record their state.
func (e *Engine) Shutdown(timeout time.Duration) {
	e.cancel()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		e.log.Warn("engine shutdown timed out")
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
	return nil, fmt.Errorf("workflow has no %s trigger", req.TriggerType)
}

func (e *Engine) execute(ctx context.Context, exec *store.Execution, req Request, start *workflow.Node) *Result {
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()

	exec.Status = store.StatusRunning
	if err := e.store.UpdateExecution(ctx, exec); err != nil {
		e.log.Error("update execution", "id", exec.ID, "err", err)
	}

	log := e.log.With("workflow", req.Workflow.ID, "execution", exec.ID)
	g := &req.Workflow.Graph
	reachable := g.Reachable(start.ID)
	order, err := g.TopoOrder(reachable)
	if err != nil {
		return e.finish(exec, nil, err)
	}

	env := expression.Env{
		"workflow":  map[string]any{"id": req.Workflow.ID, "name": req.Workflow.Name},
		"execution": map[string]any{"id": exec.ID, "triggerType": exec.TriggerType},
		"nodes":     map[string]any{},
		"trigger":   map[string]any{},
	}
	nodeOutputs := env["nodes"].(map[string]any)

	var secrets map[string]map[string]string
	if e.opts.Secrets != nil {
		if secrets, err = e.opts.Secrets.NodeSecrets(ctx, req.Workflow.ID); err != nil {
			return e.finish(exec, nil, fmt.Errorf("load secrets: %w", err))
		}
	}
	settings := map[string]moduleSettings{} // module -> settings, loaded lazily
	scrub := newScrubber(secrets)

	// inputs: node id -> input handle -> items
	inputs := map[string]map[string][]nodes.Item{start.ID: {workflow.DefaultHandle: req.Items}}
	received := map[string]bool{start.ID: true}
	var last []nodes.Item

	for _, id := range order {
		n := g.Node(id)
		step := store.Step{NodeID: n.ID, NodeName: n.Name, NodeType: n.Type, StartedAt: time.Now().UTC()}
		byHandle := inputs[id]
		in := concatInputs(byHandle)
		step.InputCount = len(in)

		if !received[id] || (id != start.ID && len(in) == 0) {
			step.Status = "skipped"
			exec.Steps = append(exec.Steps, step)
			continue
		}

		var out nodes.Output
		if n.Disabled {
			out = nodes.Output{workflow.DefaultHandle: in}
		} else {
			out, err = e.runNode(ctx, req.Workflow, withSecrets(n, secrets[n.ID]), byHandle, in, env, settings, scrub, log)
		}
		step.DurationMs = time.Since(step.StartedAt).Milliseconds()
		if err != nil {
			// Error messages are stored and shown to viewers: never include secrets.
			msg := scrub.clean(err.Error())
			step.Status = "error"
			step.Error = msg
			exec.Steps = append(exec.Steps, step)
			return e.finish(exec, nil, fmt.Errorf("node %q: %s", n.Name, msg))
		}
		step.Status = "success"
		step.Output, step.Truncated = e.truncate(out)
		exec.Steps = append(exec.Steps, step)

		all := flatten(out)
		nodeOutputs[n.Name] = all
		if id == start.ID && len(all) > 0 {
			env["trigger"] = all[0]
		}
		last = all

		for _, edge := range g.Edges {
			if edge.Source != id || !reachable[edge.Target] {
				continue
			}
			received[edge.Target] = true
			th := e.targetHandle(g, edge)
			if inputs[edge.Target] == nil {
				inputs[edge.Target] = map[string][]nodes.Item{}
			}
			inputs[edge.Target][th] = append(inputs[edge.Target][th], out[edge.Handle()]...)
		}
	}
	return e.finish(exec, last, nil)
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

func (e *Engine) runNode(ctx context.Context, wf *workflow.Workflow, n *workflow.Node, byHandle map[string][]nodes.Item,
	in []nodes.Item, env expression.Env, settings map[string]moduleSettings, scrub *scrubber, log *slog.Logger) (out nodes.Output, err error) {
	impl, ok := e.reg.Get(n.Type)
	if !ok {
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	def := impl.Definition()
	var ms moduleSettings
	if mod := def.Module; mod != "" && mod != nodes.BuiltinModule && e.opts.Secrets != nil {
		if ms, ok = settings[mod]; !ok {
			if ms.config, ms.vars, err = e.opts.Secrets.ModuleSettings(ctx, wf.OrgID, mod); err != nil {
				return nil, fmt.Errorf("load %s settings: %w", mod, err)
			}
			settings[mod] = ms
			if info, ok := e.reg.Module(mod); ok {
				for _, p := range info.Config {
					if s, isStr := ms.config[p.Name].(string); isStr && p.Type == nodes.ParamSecret {
						scrub.add(s)
					}
				}
			}
		}
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("node panic", "node", n.Name, "panic", r, "stack", string(debug.Stack()))
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
		Node: n, Def: &def, Items: in, Inputs: byHandle, Config: ms.config, Vars: ms.vars, Env: env, Logger: log,
	})
}

func (e *Engine) finish(exec *store.Execution, last []nodes.Item, err error) *Result {
	now := time.Now().UTC()
	exec.FinishedAt = &now
	if err != nil {
		exec.Status = store.StatusError
		exec.Error = err.Error()
	} else {
		exec.Status = store.StatusSuccess
	}
	// Use a fresh context so results are stored even after cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if uerr := e.store.UpdateExecution(ctx, exec); uerr != nil {
		e.log.Error("store execution result", "id", exec.ID, "err", uerr)
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
	for h, items := range out {
		if h != workflow.DefaultHandle && h != "true" && h != "false" {
			all = append(all, items...)
		}
	}
	return all
}

type moduleSettings struct{ config, vars map[string]any }

// scrubber removes known secret values from messages.
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

package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

type memStore struct {
	mu    sync.Mutex
	execs map[string]store.Execution
	n     int
}

func (m *memStore) CreateExecution(_ context.Context, e *store.Execution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	e.ID = string(rune('a' + m.n))
	m.execs[e.ID] = *e
	return nil
}

func (m *memStore) UpdateExecution(_ context.Context, e *store.Execution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs[e.ID] = *e
	return nil
}

func newEngine(t *testing.T) *Engine {
	t.Helper()
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	return New(reg, &memStore{execs: map[string]store.Execution{}}, discardLog(), Options{})
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBranchingRun(t *testing.T) {
	e := newEngine(t)
	wf := &workflow.Workflow{ID: "wf", Name: "test", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual"},
			{ID: "s", Name: "Split", Type: "core.transform", Params: map[string]any{
				"expression": "{{ map(1..4, ({n: #})) }}"}},
			{ID: "if", Name: "Even", Type: "core.if", Params: map[string]any{"condition": "{{ json.n % 2 == 0 }}"}},
			{ID: "even", Name: "Mark even", Type: "core.set", Params: map[string]any{
				"fields": map[string]any{"kind": "even", "from": "{{ trigger.who }}"}}},
			{ID: "never", Name: "Odd", Type: "core.set", Params: map[string]any{"fields": map[string]any{"x": 1}}},
		},
		Edges: []workflow.Edge{
			{ID: "1", Source: "t", Target: "s"},
			{ID: "2", Source: "s", Target: "if"},
			{ID: "3", Source: "if", SourceHandle: "true", Target: "even"},
			{ID: "4", Source: "if", SourceHandle: "false", Target: "never"},
		},
	}}
	res, err := e.Run(context.Background(), Request{
		Workflow: wf, TriggerType: nodes.TriggerManual, Items: []nodes.Item{{"who": "tester"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("status %s: %s", res.Execution.Status, res.Execution.Error)
	}
	byNode := map[string]store.Step{}
	for _, s := range res.Execution.Steps {
		byNode[s.NodeID] = s
	}
	if got := len(byNode["even"].Output["main"]); got != 2 {
		t.Fatalf("even branch got %d items", got)
	}
	if got := byNode["even"].Output["main"][0]["from"]; got != "tester" {
		t.Fatalf("trigger reference: got %v", got)
	}
	if got := len(byNode["never"].Output["main"]); got != 2 {
		t.Fatalf("odd branch got %d items", got)
	}
}

func TestSkippedBranchAndError(t *testing.T) {
	e := newEngine(t)
	wf := &workflow.Workflow{ID: "wf", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual"},
			{ID: "if", Name: "Condition", Type: "core.if", Params: map[string]any{"condition": "{{ false }}"}},
			{ID: "yes", Name: "Yes", Type: "core.log"},
			{ID: "no", Name: "No", Type: "core.set", Params: map[string]any{"fields": "{{ [1][5] }}"}},
		},
		Edges: []workflow.Edge{
			{ID: "1", Source: "t", Target: "if"},
			{ID: "2", Source: "if", SourceHandle: "true", Target: "yes"},
			{ID: "3", Source: "if", SourceHandle: "false", Target: "no"},
		},
	}}
	res, err := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]string{}
	for _, s := range res.Execution.Steps {
		steps[s.NodeID] = s.Status
	}
	if steps["yes"] != "skipped" {
		t.Fatalf("yes should be skipped, got %q", steps["yes"])
	}
	if res.Execution.Status != store.StatusError || steps["no"] != "error" {
		t.Fatalf("expected error in 'no', got %v / %v", res.Execution.Status, steps)
	}
}

func TestLogNodeWritesExecutionLog(t *testing.T) {
	e := newEngine(t)
	wf := &workflow.Workflow{ID: "wf", Name: "test", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual"},
			{ID: "s", Name: "Split", Type: "core.transform", Params: map[string]any{
				"expression": "{{ map(1..2, ({n: #})) }}"}},
			{ID: "log", Name: "Log", Type: "core.log", Params: map[string]any{
				"message": "line one\nitem {{ json.n }}", "level": "warn"}},
			{ID: "code", Name: "Code", Type: "core.code", Params: map[string]any{
				"code": "console.error('boom', 1); return items;"}},
		},
		Edges: []workflow.Edge{
			{ID: "1", Source: "t", Target: "s"},
			{ID: "2", Source: "s", Target: "log"},
			{ID: "3", Source: "log", Target: "code"},
		},
	}}
	res, err := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("status %s: %s", res.Execution.Status, res.Execution.Error)
	}
	byNode := map[string]store.Step{}
	for _, s := range res.Execution.Steps {
		byNode[s.NodeID] = s
	}
	logs := byNode["log"].Logs
	if len(logs) != 2 || logs[1].Message != "line one\nitem 2" || logs[0].Level != "warn" {
		t.Fatalf("log node: %+v", logs)
	}
	if got := len(byNode["log"].Output["main"]); got != 2 {
		t.Fatalf("log node must pass items through, got %d", got)
	}
	if cl := byNode["code"].Logs; len(cl) != 1 || cl[0].Level != "error" || cl[0].Message != "boom 1" {
		t.Fatalf("code node: %+v", cl)
	}
}

func TestLogSinkLimitsAndScrubs(t *testing.T) {
	rc := &runCtx{scrub: &scrubber{}}
	rc.scrub.add("s3cret-token")
	var logs []store.LogEntry
	sink := rc.logSink(&logs)
	sink("info", "token is s3cret-token")
	if logs[0].Message == "token is s3cret-token" {
		t.Fatal("secret not scrubbed")
	}
	for range maxStepLogs + 10 {
		sink("info", "x")
	}
	if len(logs) != maxStepLogs+1 || logs[maxStepLogs].Level != "warn" {
		t.Fatalf("got %d entries, last %+v", len(logs), logs[len(logs)-1])
	}
}

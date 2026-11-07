package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
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
			{ID: "if", Name: "If", Type: "core.if", Params: map[string]any{"condition": "{{ false }}"}},
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

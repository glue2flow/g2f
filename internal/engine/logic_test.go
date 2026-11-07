package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
)

// gateWF: Start -> A (always items) -> gate.a ; Start -> If(bOn) -> B -> gate.b
func gateWF(gate string, bOn bool, condA string) *workflow.Workflow {
	cond := "{{ false }}"
	if bOn {
		cond = "{{ true }}"
	}
	gp := map[string]any{}
	if condA != "" {
		gp["conditionA"] = condA
	}
	return &workflow.Workflow{ID: "wf", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual"},
			{ID: "a", Name: "A", Type: "core.set", Params: map[string]any{"fields": map[string]any{"src": "a", "ok": false}}},
			{ID: "if", Name: "If", Type: "core.if", Params: map[string]any{"condition": cond}},
			{ID: "b", Name: "B", Type: "core.set", Params: map[string]any{"fields": map[string]any{"src": "b"}}},
			{ID: "g", Name: "Gate", Type: "logic." + gate, Params: gp},
			{ID: "yes", Name: "Yes", Type: "core.log"},
			{ID: "no", Name: "No", Type: "core.log"},
		},
		Edges: []workflow.Edge{
			{ID: "1", Source: "t", Target: "a"},
			{ID: "2", Source: "t", Target: "if"},
			{ID: "3", Source: "if", SourceHandle: "true", Target: "b"},
			{ID: "4", Source: "a", Target: "g", TargetHandle: "a"},
			{ID: "5", Source: "b", Target: "g", TargetHandle: "b"},
			{ID: "6", Source: "g", SourceHandle: "true", Target: "yes"},
			{ID: "7", Source: "g", SourceHandle: "false", Target: "no"},
		},
	}}
}

func TestLogicGates(t *testing.T) {
	cases := []struct {
		gate     string
		bOn      bool
		condA    string
		wantTrue int // items on "true"; -1 means the false branch ran
	}{
		{"and", true, "", 2},
		{"and", false, "", -1},
		{"or", false, "", 1},
		{"or", true, "", 2},
		{"xor", true, "", -1},
		{"xor", false, "", 1},
		{"and", true, "{{ json.ok }}", -1}, // A has items, but none match
		{"xor", true, "{{ json.ok }}", 1},  // only B is true
	}
	for _, c := range cases {
		e := newEngine(t)
		res, err := e.Run(context.Background(), Request{Workflow: gateWF(c.gate, c.bOn, c.condA), TriggerType: nodes.TriggerManual})
		if err != nil {
			t.Fatal(err)
		}
		if res.Execution.Status != store.StatusSuccess {
			t.Fatalf("%+v: %s", c, res.Execution.Error)
		}
		steps := map[string]store.Step{}
		for _, s := range res.Execution.Steps {
			steps[s.NodeID] = s
		}
		if c.wantTrue < 0 {
			if steps["yes"].Status != "skipped" || steps["no"].Status != "success" {
				t.Errorf("%+v: expected false branch, got yes=%s no=%s", c, steps["yes"].Status, steps["no"].Status)
			}
			continue
		}
		if got := len(steps["yes"].Output["main"]); got != c.wantTrue || steps["no"].Status != "skipped" {
			t.Errorf("%+v: true items %d (want %d), no=%s", c, got, c.wantTrue, steps["no"].Status)
		}
	}
}

type fakeSecrets map[string]map[string]string

func (f fakeSecrets) NodeSecrets(context.Context, string) (map[string]map[string]string, error) {
	return f, nil
}
func (f fakeSecrets) ModuleConfig(context.Context, string, string) (map[string]any, error) {
	return nil, errors.New("unused")
}

func TestSecretsInjectedAndScrubbed(t *testing.T) {
	reg := nodes.NewRegistry()
	_ = nodes.RegisterBuiltins(reg)
	st := &memStore{execs: map[string]store.Execution{}}
	e := New(reg, st, discardLog(), Options{Secrets: fakeSecrets{"h": {"secret": "sup3r-s3cret"}}})
	// The HTTP node fails with an error that echoes the secret value.
	wf := &workflow.Workflow{ID: "wf", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual"},
			{ID: "h", Name: "Leaky", Type: "core.http", Params: map[string]any{"url": "not a url sup3r-s3cret"}},
		},
		Edges: []workflow.Edge{{ID: "1", Source: "t", Target: "h"}},
	}}
	res, _ := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
	if res.Execution.Status != store.StatusError {
		t.Fatal("expected error")
	}
	if strings.Contains(res.Execution.Error, "sup3r-s3cret") || strings.Contains(res.Execution.Steps[1].Error, "sup3r-s3cret") {
		t.Fatalf("secret not scrubbed: %s", res.Execution.Error)
	}
}

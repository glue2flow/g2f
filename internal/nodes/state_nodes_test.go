package nodes

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

// exec runs a node like the engine does (trigger type "schedule" unless set).
func runNode(t *testing.T, n Node, params map[string]any, items []Item, state StateStore, trigger ...string) Output {
	t.Helper()
	out, err := runNodeErr(n, params, items, state, trigger...)
	if err != nil {
		t.Fatalf("%s: %v", n.Definition().Type, err)
	}
	return out
}

func runNodeErr(n Node, params map[string]any, items []Item, state StateStore, trigger ...string) (Output, error) {
	def := n.Definition()
	tt := "schedule"
	if len(trigger) > 0 {
		tt = trigger[0]
	}
	if params == nil {
		params = map[string]any{}
	}
	return n.Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{ID: "n1", Name: def.Name, Params: params}, Def: &def, Items: items,
		Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), State: state, TriggerType: tt,
	})
}

func TestDataStore(t *testing.T) {
	st := NewMemoryState()
	n := dataStoreNode{}
	out := runNode(t, n, map[string]any{"operation": "get", "key": "last", "defaultValue": 0}, []Item{{"a": 1}}, st)
	if got := out["main"][0]; got["value"] != 0 || got["a"] != 1 {
		t.Fatalf("default: %v", got)
	}
	runNode(t, n, map[string]any{"operation": "set", "key": "last", "value": "{{ json.id }}"}, []Item{{"id": 41}, {"id": 42}}, st)
	out = runNode(t, n, map[string]any{"operation": "get", "key": "last", "resultField": "lastId"}, []Item{{}}, st)
	if out["main"][0]["lastId"] != 42.0 {
		t.Fatalf("get: %v", out["main"])
	}
	for range 3 {
		out = runNode(t, n, map[string]any{"operation": "increment", "key": "runs", "by": 2}, []Item{{}}, st)
	}
	if out["main"][0]["value"] != 6.0 {
		t.Fatalf("increment: %v", out["main"])
	}
	out = runNode(t, n, map[string]any{"operation": "list"}, []Item{{}, {}}, st)
	if len(out["main"]) != 2 || out["main"][0]["key"] != "last" {
		t.Fatalf("list (runs once): %v", out["main"])
	}
	out = runNode(t, n, map[string]any{"operation": "delete", "key": "{{ json.k }}"}, []Item{{"k": "runs"}, {"k": "nope"}}, st)
	if out["main"][0]["value"] != true || out["main"][1]["value"] != false {
		t.Fatalf("delete: %v", out["main"])
	}
	if out = runNode(t, n, map[string]any{"operation": "clear"}, []Item{{}}, st); out["main"][0]["cleared"] != int64(1) {
		t.Fatalf("clear: %v", out["main"])
	}
	if _, err := runNodeErr(n, map[string]any{"operation": "get", "key": "x"}, []Item{{}}, nil); err != ErrNoState {
		t.Fatalf("without state: %v", err)
	}
}

func TestRemoveDuplicates(t *testing.T) {
	st := NewMemoryState()
	n := dedupeNode{}
	ids := func(items []Item) (out []any) {
		for _, it := range items {
			out = append(out, it["id"])
		}
		return out
	}
	// Within the batch: the second 1 is a duplicate.
	out := runNode(t, n, nil, []Item{{"id": 1}, {"id": 2}, {"id": 1}}, st)
	if len(out["main"]) != 2 || len(out["duplicates"]) != 1 {
		t.Fatalf("first run: %v / %v", ids(out["main"]), ids(out["duplicates"]))
	}
	// A test run shows only 3 as new and records nothing.
	out = runNode(t, n, nil, []Item{{"id": 2}, {"id": 3}}, st, "manual")
	if len(out["main"]) != 1 || out["main"][0]["id"] != 3 {
		t.Fatalf("test run: %v", ids(out["main"]))
	}
	// A real run: 3 is still new, 2 is known.
	out = runNode(t, n, nil, []Item{{"id": 2}, {"id": 3}}, st)
	if len(out["main"]) != 1 || out["main"][0]["id"] != 3 {
		t.Fatalf("second run: %v", ids(out["main"]))
	}
	if out = runNode(t, n, nil, []Item{{"id": 3}}, st); len(out["main"]) != 0 {
		t.Fatal("3 passed twice")
	}
	// Separate history names do not share state; within-run mode ignores history.
	if out = runNode(t, n, map[string]any{"history": "other"}, []Item{{"id": 3}}, st); len(out["main"]) != 1 {
		t.Fatal("history name not separate")
	}
	if out = runNode(t, n, map[string]any{"scope": "run", "key": "{{ json }}"}, []Item{{"id": 3}, {"id": 3}}, nil); len(out["main"]) != 1 {
		t.Fatalf("within run: %v", out)
	}
	if _, err := runNodeErr(n, map[string]any{"key": "{{ json.missing }}"}, []Item{{}}, st); err == nil {
		t.Fatal("empty key accepted")
	}
}

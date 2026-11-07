package nodes

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/wolfposd/g8g/internal/expression"
	"github.com/wolfposd/g8g/internal/workflow"
)

func run(t *testing.T, n Node, params map[string]any, inputs map[string][]Item) (Output, error) {
	t.Helper()
	def := n.Definition()
	var all []Item
	for _, h := range []string{"main", "a", "b"} {
		all = append(all, inputs[h]...)
	}
	return n.Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{ID: "n", Name: "N", Params: params}, Def: &def,
		Items: all, Inputs: inputs,
		Env:    expression.Env{"nodes": map[string]any{}, "workflow": map[string]any{"name": "wf"}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestSwitch(t *testing.T) {
	def := switchNode{}.Definition()
	if got := def.OutputsFor(map[string]any{"cases": "a, b ,a,"}); !reflect.DeepEqual(got, []string{"a", "b", "default"}) {
		t.Fatalf("outputs %v", got)
	}
	out, err := run(t, switchNode{}, map[string]any{"value": "{{ json.s }}", "cases": "Critical, warning", "ignoreCase": true},
		map[string][]Item{"main": {{"s": "critical"}, {"s": "warning"}, {"s": "info"}, {"s": nil}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out["Critical"]) != 1 || len(out["warning"]) != 1 || len(out["default"]) != 2 {
		t.Fatalf("routing %v", out)
	}
}

func TestMerge(t *testing.T) {
	a := []Item{{"id": 1, "name": "Ada"}, {"id": 2, "name": "Bob"}}
	b := []Item{{"id": 2, "plan": "pro"}, {"id": 3, "plan": "free"}}
	in := map[string][]Item{"a": a, "b": b}
	cases := []struct {
		params map[string]any
		want   int
	}{
		{map[string]any{"mode": "append"}, 4},
		{map[string]any{"mode": "byKey", "join": "inner"}, 1},
		{map[string]any{"mode": "byKey", "join": "left"}, 2},
		{map[string]any{"mode": "byKey", "join": "outer"}, 3},
		{map[string]any{"mode": "byPosition", "join": "inner"}, 2},
	}
	for _, c := range cases {
		out, err := run(t, mergeNode{}, c.params, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(out["main"]) != c.want {
			t.Errorf("%v: %d items, want %d: %v", c.params, len(out["main"]), c.want, out["main"])
		}
	}
	out, _ := run(t, mergeNode{}, map[string]any{"mode": "byKey"}, in)
	if got := out["main"][0]; got["name"] != "Bob" || got["plan"] != "pro" {
		t.Fatalf("joined item %v", got)
	}
}

func TestCodeNode(t *testing.T) {
	items := []Item{{"n": 1.0}, {"n": 2.0}, {"n": 3.0}}
	out, err := run(t, codeNode{}, map[string]any{
		"code": "return items.filter(i => i.n > 1).map(i => ({ n: i.n * 10, wf: workflow.name }));",
	}, map[string][]Item{"main": items})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Item{{"n": 20.0, "wf": "wf"}, {"n": 30.0, "wf": "wf"}}; !reflect.DeepEqual(out["main"], want) {
		t.Fatalf("got %v", out["main"])
	}

	// Per item; null drops; mutations must not leak into the input items.
	out, err = run(t, codeNode{}, map[string]any{
		"mode": "each", "code": "item.n = item.n + 100; return index === 1 ? null : item;",
	}, map[string][]Item{"main": items})
	if err != nil {
		t.Fatal(err)
	}
	if len(out["main"]) != 2 || out["main"][0]["n"] != 101.0 || items[0]["n"] != 1.0 {
		t.Fatalf("each mode: %v (input %v)", out["main"], items)
	}

	// Errors carry the JavaScript message.
	_, err = run(t, codeNode{}, map[string]any{"code": "return missing.x;"}, map[string][]Item{"main": items})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected ReferenceError, got %v", err)
	}

	// Infinite loops are interrupted.
	_, err = run(t, codeNode{}, map[string]any{"code": "while (true) {}", "timeoutSeconds": 0.2}, map[string][]Item{"main": items})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}

	// No access to the host: no require, no process.
	_, err = run(t, codeNode{}, map[string]any{"code": "return require('fs');"}, map[string][]Item{"main": items})
	if err == nil {
		t.Fatal("require must not exist")
	}

	CodeNodeEnabled = false
	defer func() { CodeNodeEnabled = true }()
	if _, err := run(t, codeNode{}, map[string]any{"code": "return []"}, nil); err == nil {
		t.Fatal("disabled code node must fail")
	}
}

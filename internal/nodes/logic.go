package nodes

import (
	"context"
	"fmt"
)

// Logic gates combine two branches of a workflow.
//
// Each gate has the inputs "a" and "b". An input is true when at least one of
// its items satisfies the input's condition (default: the input received any
// item at all). The gate's result decides where items go:
//
//	true  -> items of the inputs that were true (And: a+b, Xor: the single true input)
//	false -> all received items
//
// Because the engine runs a node only after all its predecessors, gates also
// act as joins: And fires "true" once both branches produced matching items.
type gateOp string

const (
	gateAnd gateOp = "and"
	gateOr  gateOp = "or"
	gateXor gateOp = "xor"
)

type gateNode struct{ op gateOp }

func (g gateNode) Definition() Definition {
	var name, desc string
	switch g.op {
	case gateAnd:
		name, desc = "And", "True when both input A and input B are true. Use it to continue only when two branches both produced data."
	case gateOr:
		name, desc = "Or", "True when input A or input B (or both) are true. Merges two branches."
	case gateXor:
		name, desc = "Xor", "True when exactly one of input A and input B is true."
	}
	return Definition{
		Type: "logic." + string(g.op), Name: name, Category: "Logic", Source: "builtin", Module: BuiltinModule,
		Description: desc + " An input is true if it received an item matching its condition.",
		Color:       "#ec4899", Inputs: []string{"a", "b"}, Outputs: []string{"true", "false"},
		Params: []Param{
			{Name: "conditionA", Label: "Input A is true when", Type: ParamString, Default: "{{ true }}",
				Placeholder: "{{ json.status == 'ok' }}",
				Description: "Evaluated per item of input A; A is true if any item matches. Default: A received items."},
			{Name: "conditionB", Label: "Input B is true when", Type: ParamString, Default: "{{ true }}",
				Placeholder: "{{ json.approved }}"},
		},
	}
}

func (g gateNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	a, b := ec.Inputs["a"], ec.Inputs["b"]
	aTrue, err := anyMatch(ec, "conditionA", a)
	if err != nil {
		return nil, err
	}
	bTrue, err := anyMatch(ec, "conditionB", b)
	if err != nil {
		return nil, err
	}

	var result bool
	switch g.op {
	case gateAnd:
		result = aTrue && bTrue
	case gateOr:
		result = aTrue || bTrue
	case gateXor:
		result = aTrue != bTrue
	default:
		return nil, fmt.Errorf("unknown gate %q", g.op)
	}

	out := Output{"true": {}, "false": {}}
	if !result {
		out["false"] = append(append(out["false"], a...), b...)
		return out, nil
	}
	if aTrue {
		out["true"] = append(out["true"], a...)
	}
	if bTrue {
		out["true"] = append(out["true"], b...)
	}
	return out, nil
}

func anyMatch(ec *ExecContext, param string, items []Item) (bool, error) {
	for i, item := range items {
		ok, err := ec.Bool(param, item, i)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

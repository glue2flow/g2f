package nodes

import (
	"context"
	"fmt"
	"strings"

	"github.com/wolfposd/g8g/internal/expression"
)

// --- Switch -----------------------------------------------------------------

type switchNode struct{}

func (switchNode) Definition() Definition {
	d := Definition{
		Type: "core.switch", Name: "Switch", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Route each item to the output whose name equals the value; items without a match go to 'default'.",
		Color:       "#f97316", Inputs: []string{"main"},
		DynamicOutputs: &DynamicOutputs{Param: "cases", Extra: []string{"default"}},
		Params: []Param{
			{Name: "value", Label: "Value", Type: ParamString, Required: true, Default: "{{ json.severity }}",
				Description: "Evaluated per item and compared with the cases."},
			{Name: "cases", Label: "Cases (outputs)", Type: ParamString, Default: "critical, warning, info", NoExpression: true,
				Description: "Comma separated; each case becomes an output. Unmatched items go to 'default'."},
			{Name: "ignoreCase", Label: "Ignore case", Type: ParamBoolean, Default: false},
		},
	}
	d.Outputs = d.OutputsFor(nil)
	return d
}

func (switchNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	cases := ParseList(ec.Raw("cases"))
	ignoreCase, _ := ec.Raw("ignoreCase").(bool)
	out := Output{"default": {}}
	for _, c := range cases {
		out[c] = []Item{}
	}
	for i, item := range ec.Items {
		v, err := ec.Param("value", item, i)
		if err != nil {
			return nil, err
		}
		s := expression.Stringify(v)
		target := "default"
		for _, c := range cases {
			if s == c || (ignoreCase && strings.EqualFold(s, c)) {
				target = c
				break
			}
		}
		out[target] = append(out[target], item)
	}
	return out, nil
}

// --- Merge ------------------------------------------------------------------

type mergeNode struct{}

func (mergeNode) Definition() Definition {
	return Definition{
		Type: "core.merge", Name: "Merge", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Combine the items of inputs A and B: append, join by a key, or pair by position.",
		Color:       "#14b8a6", Inputs: []string{"a", "b"}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "mode", Label: "Mode", Type: ParamSelect, Default: "append", NoExpression: true, Options: []Option{
				{"Append (A then B)", "append"}, {"Join by key", "byKey"}, {"Pair by position", "byPosition"},
			}},
			{Name: "keyA", Label: "Key of A", Type: ParamString, Default: "{{ json.id }}",
				ShowIf: map[string][]string{"mode": {"byKey"}}, Description: "Evaluated per item of input A."},
			{Name: "keyB", Label: "Key of B", Type: ParamString, Default: "{{ json.id }}",
				ShowIf: map[string][]string{"mode": {"byKey"}}, Description: "Evaluated per item of input B."},
			{Name: "join", Label: "Keep", Type: ParamSelect, Default: "inner", NoExpression: true,
				ShowIf: map[string][]string{"mode": {"byKey", "byPosition"}}, Options: []Option{
					{"Only matches (inner)", "inner"}, {"All of A (left)", "left"}, {"Everything (outer)", "outer"},
				}},
		},
	}
}

func (mergeNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	a, b := ec.Inputs["a"], ec.Inputs["b"]
	mode, _ := ec.Raw("mode").(string)
	join, _ := ec.Raw("join").(string)
	switch mode {
	case "", "append":
		return Output{"main": append(append([]Item{}, a...), b...)}, nil
	case "byPosition":
		n := min(len(a), len(b))
		switch join {
		case "left":
			n = len(a)
		case "outer":
			n = max(len(a), len(b))
		}
		out := make([]Item, 0, n)
		for i := range n {
			out = append(out, mergeItems(at(a, i), at(b, i)))
		}
		return Output{"main": out}, nil
	case "byKey":
		return mergeByKey(ec, a, b, join)
	}
	return nil, fmt.Errorf("unknown mode %q", mode)
}

func mergeByKey(ec *ExecContext, a, b []Item, join string) (Output, error) {
	keys := func(param string, items []Item) ([]string, error) {
		out := make([]string, len(items))
		for i, item := range items {
			v, err := ec.Param(param, item, i)
			if err != nil {
				return nil, err
			}
			out[i] = expression.Stringify(v)
		}
		return out, nil
	}
	ka, err := keys("keyA", a)
	if err != nil {
		return nil, err
	}
	kb, err := keys("keyB", b)
	if err != nil {
		return nil, err
	}
	index := map[string][]int{}
	for i, k := range kb {
		index[k] = append(index[k], i)
	}
	usedB := make([]bool, len(b))
	out := []Item{}
	for i, k := range ka {
		matches := index[k]
		if len(matches) == 0 {
			if join == "left" || join == "outer" {
				out = append(out, mergeItems(a[i], nil))
			}
			continue
		}
		for _, j := range matches {
			usedB[j] = true
			out = append(out, mergeItems(a[i], b[j]))
		}
	}
	if join == "outer" {
		for j, used := range usedB {
			if !used {
				out = append(out, mergeItems(nil, b[j]))
			}
		}
	}
	return Output{"main": out}, nil
}

func at(items []Item, i int) Item {
	if i < len(items) {
		return items[i]
	}
	return nil
}

// mergeItems combines two items; fields of b win on conflicts.
func mergeItems(a, b Item) Item {
	out := make(Item, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

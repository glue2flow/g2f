package nodes

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
)

// Item list nodes (category "Data"): filter, sort, limit, split, aggregate,
// map and edit fields. All are pure functions of their input items.

func dataDef(typ, name, desc string, outputs []string, params []Param, schema *OutputSchema) Definition {
	return Definition{
		Type: typ, Name: name, Category: "Data", Source: "builtin", Module: BuiltinModule, Description: desc,
		Color: "#0891b2", Inputs: []string{"main"}, Outputs: outputs, Params: params, OutputSchema: schema,
	}
}

// --- Filter -----------------------------------------------------------------

type filterNode struct{}

func (filterNode) Definition() Definition {
	return dataDef("data.filter", "Filter", "Keep the items that match a condition. The others go to the 'discarded' output.",
		[]string{"main", "discarded"},
		[]Param{{Name: "condition", Label: "Keep items where", Required: true, Default: "{{ json.active == true }}",
			Placeholder: "{{ json.total > 100 && json.country == 'DE' }}"}},
		passthroughSchema)
}

func (filterNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	out := Output{"main": {}, "discarded": {}}
	for i, item := range ec.Items {
		ok, err := ec.Bool("condition", item, i)
		if err != nil {
			return nil, err
		}
		if ok {
			out["main"] = append(out["main"], item)
		} else {
			out["discarded"] = append(out["discarded"], item)
		}
	}
	return out, nil
}

// --- Sort -------------------------------------------------------------------

type sortNode struct{}

func (sortNode) Definition() Definition {
	return dataDef("data.sort", "Sort", "Sort items by a value. Numbers sort numerically, text alphabetically "+
		"(ignoring case), dates chronologically. An array sorts by several values: {{ [json.lastName, json.firstName] }}.",
		[]string{"main"},
		[]Param{
			{Name: "by", Label: "Sort by", Required: true, Default: "{{ json.name }}"},
			{Name: "order", Label: "Order", Type: ParamSelect, Default: "asc", NoExpression: true, Options: []Option{
				{"Ascending (A→Z, 1→9, oldest first)", "asc"}, {"Descending", "desc"},
			}},
		},
		passthroughSchema)
}

func (sortNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	keys := make([]any, len(ec.Items))
	for i, item := range ec.Items {
		v, err := ec.Param("by", item, i)
		if err != nil {
			return nil, err
		}
		keys[i] = v
	}
	idx := make([]int, len(ec.Items))
	for i := range idx {
		idx[i] = i
	}
	desc, _ := ec.Raw("order").(string)
	sort.SliceStable(idx, func(a, b int) bool {
		c := compareValues(keys[idx[a]], keys[idx[b]])
		if desc == "desc" {
			return c > 0
		}
		return c < 0
	})
	out := make([]Item, len(idx))
	for i, j := range idx {
		out[i] = ec.Items[j]
	}
	return Output{"main": out}, nil
}

// compareValues orders values of mixed types: numbers numerically, strings
// case-insensitively (numeric strings as numbers, RFC 3339 dates by time),
// booleans false < true, arrays element by element. nil sorts last.
func compareValues(a, b any) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return 1
		default:
			return -1
		}
	}
	if la, ok := a.([]any); ok {
		if lb, ok := b.([]any); ok {
			for i := 0; i < len(la) && i < len(lb); i++ {
				if c := compareValues(la[i], lb[i]); c != 0 {
					return c
				}
			}
			return len(la) - len(lb)
		}
	}
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			switch {
			case fa < fb:
				return -1
			case fa > fb:
				return 1
			}
			return 0
		}
	}
	if ta, ok := toTime(a); ok {
		if tb, ok := toTime(b); ok {
			return ta.Compare(tb)
		}
	}
	if ba, ok := a.(bool); ok {
		if bb, ok := b.(bool); ok {
			switch {
			case ba == bb:
				return 0
			case !ba:
				return -1
			}
			return 1
		}
	}
	sa, sb := strings.ToLower(expression.Stringify(a)), strings.ToLower(expression.Stringify(b))
	return strings.Compare(sa, sb)
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil && !math.IsNaN(f)
	}
	return 0, false
}

func toTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04", "2006-01-02"} {
			if tm, err := time.Parse(layout, t); err == nil {
				return tm, true
			}
		}
	}
	return time.Time{}, false
}

// --- Limit ------------------------------------------------------------------

type limitNode struct{}

func (limitNode) Definition() Definition {
	return dataDef("data.limit", "Limit", "Keep only the first (or last) N items.", []string{"main"},
		[]Param{
			{Name: "max", Label: "Max items", Type: ParamNumber, Default: 10, NoExpression: true},
			{Name: "keep", Label: "Keep", Type: ParamSelect, Default: "first", NoExpression: true, Options: []Option{
				{"First items", "first"}, {"Last items", "last"},
			}},
		},
		passthroughSchema)
}

func (limitNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	n, err := ec.Number("max", nil, 0)
	if err != nil {
		return nil, err
	}
	m := max(int(n), 0)
	items := ec.Items
	if len(items) > m {
		if keep, _ := ec.Raw("keep").(string); keep == "last" {
			items = items[len(items)-m:]
		} else {
			items = items[:m]
		}
	}
	return Output{"main": items}, nil
}

// --- Split Out --------------------------------------------------------------

type splitOutNode struct{}

func (splitOutNode) Definition() Definition {
	return dataDef("data.splitOut", "Split Out", "Turn a list inside each item into separate items, "+
		"e.g. one item per attachment or per order line.", []string{"main"},
		[]Param{
			{Name: "list", Label: "List", Required: true, Default: "{{ json.items }}"},
			{Name: "include", Label: "Each new item contains", Type: ParamSelect, Default: "element", NoExpression: true, Options: []Option{
				{"The list element only", "element"}, {"The element plus the other fields of the item", "withParent"},
			}},
			{Name: "field", Label: "Field name", Default: "item", NoExpression: true,
				Description: "Holds the element when the other fields are kept, or when elements are not objects."},
		},
		dynamicSchema)
}

func (splitOutNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	include, _ := ec.Raw("include").(string)
	field, _ := ec.Raw("field").(string)
	if field = strings.TrimSpace(field); field == "" {
		field = "item"
	}
	out := []Item{}
	for i, item := range ec.Items {
		v, err := ec.Param("list", item, i)
		if err != nil {
			return nil, err
		}
		var list []any
		switch t := v.(type) {
		case nil:
		case []any:
			list = t
		case []Item:
			for _, e := range t {
				list = append(list, e)
			}
		default:
			list = []any{t}
		}
		for _, el := range list {
			if include == "withParent" {
				next := make(Item, len(item)+1)
				maps.Copy(next, item)
				next[field] = el
				out = append(out, next)
				continue
			}
			if m, ok := el.(map[string]any); ok {
				out = append(out, m)
			} else {
				out = append(out, Item{field: el})
			}
		}
	}
	return Output{"main": out}, nil
}

// --- Aggregate --------------------------------------------------------------

type aggregateNode struct{}

var aggOps = []Option{
	{"Collect the items into a list", "items"},
	{"Collect a value into a list", "values"},
	{"Count", "count"},
	{"Sum", "sum"},
	{"Average", "average"},
	{"Minimum", "min"},
	{"Maximum", "max"},
	{"Join as text", "join"},
	{"Unique values", "unique"},
	{"First value", "first"},
	{"Last value", "last"},
}

func (aggregateNode) Definition() Definition {
	valueOps := map[string][]string{"operation": {"values", "sum", "average", "min", "max", "join", "unique", "first", "last"}}
	return dataDef("data.aggregate", "Aggregate", "Combine many items into one, or one per group: "+
		"collect them into a list, count, sum, average, join text, …", []string{"main"},
		[]Param{
			{Name: "operation", Label: "Operation", Type: ParamSelect, Default: "items", NoExpression: true, Options: aggOps},
			{Name: "value", Label: "Value", Default: "{{ json.amount }}", ShowIf: valueOps},
			{Name: "separator", Label: "Separator", Default: ", ", NoExpression: true, ShowIf: map[string][]string{"operation": {"join"}}},
			{Name: "groupBy", Label: "Group by", Placeholder: "{{ json.customerId }}",
				Description: "Optional: one output item per distinct value, with the field 'group'."},
			{Name: "field", Label: "Result field", Default: "result", NoExpression: true},
		},
		&OutputSchema{Fields: fields(
			"result:any:Result (default result field)",
			"count:number:Number of items",
			"group:any:Group value (with Group by)",
		)})
}

func (aggregateNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	op, _ := ec.Raw("operation").(string)
	if op == "" {
		op = "items"
	}
	field, _ := ec.Raw("field").(string)
	if field = strings.TrimSpace(field); field == "" {
		field = "result"
	}
	sep, _ := ec.Raw("separator").(string)
	_, grouped := ec.Raw("groupBy").(string)
	grouped = grouped && strings.TrimSpace(ec.Raw("groupBy").(string)) != ""

	type group struct {
		key    any
		items  []Item
		values []any
	}
	var order []string
	groups := map[string]*group{}
	for i, item := range ec.Items {
		var gkey any
		if grouped {
			v, err := ec.Param("groupBy", item, i)
			if err != nil {
				return nil, err
			}
			gkey = v
		}
		id := expression.Stringify(gkey)
		g := groups[id]
		if g == nil {
			g = &group{key: gkey}
			groups[id] = g
			order = append(order, id)
		}
		g.items = append(g.items, item)
		if op != "items" && op != "count" {
			v, err := ec.Param("value", item, i)
			if err != nil {
				return nil, err
			}
			g.values = append(g.values, v)
		}
	}
	if len(order) == 0 && !grouped {
		order, groups[""] = []string{""}, &group{}
	}

	out := make([]Item, 0, len(order))
	for _, id := range order {
		g := groups[id]
		res, err := aggregate(op, g.items, g.values, sep)
		if err != nil {
			return nil, err
		}
		it := Item{field: res, "count": len(g.items)}
		if grouped {
			it["group"] = g.key
		}
		out = append(out, it)
	}
	return Output{"main": out}, nil
}

func aggregate(op string, items []Item, values []any, sep string) (any, error) {
	nums := func() ([]float64, error) {
		var out []float64
		for _, v := range values {
			if v == nil || v == "" {
				continue // empty values are skipped
			}
			f, ok := toFloat(v)
			if !ok {
				return nil, fmt.Errorf("%s: %v is not a number", op, v)
			}
			out = append(out, f)
		}
		return out, nil
	}
	switch op {
	case "items":
		list := make([]any, len(items))
		for i, it := range items {
			list[i] = it
		}
		return list, nil
	case "values":
		return append([]any{}, values...), nil
	case "count":
		return len(items), nil
	case "sum", "average":
		ns, err := nums()
		if err != nil {
			return nil, err
		}
		sum := 0.0
		for _, f := range ns {
			sum += f
		}
		if op == "average" {
			if len(ns) == 0 {
				return nil, nil
			}
			return sum / float64(len(ns)), nil
		}
		return sum, nil
	case "min", "max":
		var best any
		for _, v := range values {
			if v == nil {
				continue
			}
			if c := compareValues(v, best); best == nil || (op == "min" && c < 0) || (op == "max" && c > 0) {
				best = v
			}
		}
		return best, nil
	case "join":
		parts := make([]string, 0, len(values))
		for _, v := range values {
			if s := expression.Stringify(v); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, sep), nil
	case "unique":
		seen := map[string]bool{}
		out := []any{}
		for _, v := range values {
			k := expression.Stringify(v)
			if !seen[k] {
				seen[k] = true
				out = append(out, v)
			}
		}
		return out, nil
	case "first", "last":
		if len(values) == 0 {
			return nil, nil
		}
		if op == "first" {
			return values[0], nil
		}
		return values[len(values)-1], nil
	}
	return nil, fmt.Errorf("unknown operation %q", op)
}

// --- Map --------------------------------------------------------------------

type mapNode struct{}

func (mapNode) Definition() Definition {
	return dataDef("data.map", "Map", "Turn each item into a new item with one expression. Return an object, "+
		"a list (several items) or null (drop the item).", []string{"main"},
		[]Param{{Name: "expression", Label: "New item", Type: ParamText, Required: true,
			Default: "{{ {name: json.firstName + ' ' + json.lastName, email: lower(json.email)} }}"}},
		dynamicSchema)
}

func (mapNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	out := []Item{}
	for i, item := range ec.Items {
		v, err := ec.Param("expression", item, i)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		out = append(out, ToItems(v)...)
	}
	return Output{"main": out}, nil
}

// --- Edit Fields ------------------------------------------------------------

type editFieldsNode struct{}

func (editFieldsNode) Definition() Definition {
	return dataDef("data.editFields", "Rename / Remove Fields", "Rename, remove or keep only some fields of each item. "+
		"Paths may be nested: customer.address.city.", []string{"main"},
		[]Param{
			{Name: "rename", Label: "Rename", Type: ParamJSON, NoExpression: true,
				Description: `Old path → new path, e.g. {"mail": "email", "addr.zip": "postalCode"}.`},
			{Name: "remove", Label: "Remove", NoExpression: true, Placeholder: "password, internal.notes",
				Description: "Comma separated paths."},
			{Name: "keepOnly", Label: "Keep only", NoExpression: true, Placeholder: "id, email, name",
				Description: "Comma separated paths; all other fields are dropped (applied after renaming)."},
		},
		&OutputSchema{Passthrough: true})
}

func (editFieldsNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	rename, err := toMap(ec.Raw("rename"))
	if err != nil {
		return nil, fmt.Errorf("rename: %w", err)
	}
	renames := make([]string, 0, len(rename))
	for k := range rename {
		renames = append(renames, k)
	}
	sort.Strings(renames)
	remove := ParseList(ec.Raw("remove"))
	keep := ParseList(ec.Raw("keepOnly"))

	out := make([]Item, len(ec.Items))
	for i, item := range ec.Items {
		next := deepCopy(item).(map[string]any)
		for _, from := range renames {
			to := expression.Stringify(rename[from])
			if v, ok := getPath(next, from); ok && to != "" {
				deletePath(next, from)
				setPath(next, to, v)
			}
		}
		for _, p := range remove {
			deletePath(next, p)
		}
		if len(keep) > 0 {
			kept := Item{}
			for _, p := range keep {
				if v, ok := getPath(next, p); ok {
					setPath(kept, p, v)
				}
			}
			next = kept
		}
		out[i] = next
	}
	return Output{"main": out}, nil
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

func getPath(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(m map[string]any, path string, v any) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

func deletePath(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, parts[len(parts)-1])
}

package nodes

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
)

const day = 24 * time.Hour

// ttlParam reads a "keep for (days)" parameter; 0 means forever.
func ttlParam(ec *ExecContext, name string, item Item, i int) (time.Duration, error) {
	d, err := ec.Number(name, item, i)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return time.Duration(d * float64(day)), nil
}

// --- Data Store -------------------------------------------------------------

type dataStoreNode struct{}

const dataScope = "data"

func (dataStoreNode) Definition() Definition {
	keyOps := map[string][]string{"operation": {"get", "set", "increment", "delete"}}
	return Definition{
		Type: "data.store", Name: "Data Store", Category: "Data", Source: "builtin", Module: BuiltinModule,
		Description: "Keep values between runs of this workflow: counters, the last processed ID, a cache. " +
			"Values are JSON and belong to the workflow.",
		Color: "#0d9488", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "operation", Label: "Operation", Type: ParamSelect, Default: "get", NoExpression: true, Options: []Option{
				{"Get a value", "get"}, {"Set a value", "set"}, {"Increment a number", "increment"},
				{"Delete a value", "delete"}, {"List values", "list"}, {"Delete all values", "clear"},
			}},
			{Name: "key", Label: "Key", Required: true, Placeholder: "lastInvoiceId", ShowIf: keyOps},
			{Name: "value", Label: "Value", Type: ParamJSON, Default: "{{ json.id }}", ShowIf: map[string][]string{"operation": {"set"}},
				Description: "Any JSON value; expressions refer to the current item."},
			{Name: "by", Label: "Increment by", Type: ParamNumber, Default: 1, ShowIf: map[string][]string{"operation": {"increment"}}},
			{Name: "defaultValue", Label: "Default", Type: ParamJSON, ShowIf: map[string][]string{"operation": {"get"}},
				Description: "Returned when the key does not exist."},
			{Name: "ttlDays", Label: "Keep for (days)", Type: ParamNumber, Default: 0,
				ShowIf: map[string][]string{"operation": {"set", "increment"}}, Description: "0 keeps the value until it is deleted."},
			{Name: "prefix", Label: "Key prefix", ShowIf: map[string][]string{"operation": {"list"}}},
			{Name: "resultField", Label: "Result field", Default: "value", NoExpression: true, ShowIf: keyOps,
				Description: "Field of the output item that receives the value (delete: whether it existed)."},
		},
		OutputSchema: &OutputSchema{Passthrough: true, Fields: fields(
			"value:any:Stored value (default result field)",
			"key:string:List values: the key",
			"cleared:number:Delete all values: number of deleted values",
		)},
	}
}

func (dataStoreNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	if ec.State == nil {
		return nil, ErrNoState
	}
	op, _ := ec.Raw("operation").(string)
	switch op {
	case "list":
		prefix, err := ec.String("prefix", first(ec.Items), 0)
		if err != nil {
			return nil, err
		}
		list, err := ec.State.List(ctx, dataScope, prefix, 1000)
		if err != nil {
			return nil, err
		}
		out := make([]Item, len(list))
		for i, e := range list {
			out[i] = Item{"key": e.Key, "value": e.Value, "updatedAt": e.UpdatedAt.UTC().Format(time.RFC3339)}
		}
		return Output{"main": out}, nil
	case "clear":
		n, err := ec.State.Clear(ctx, dataScope)
		if err != nil {
			return nil, err
		}
		ec.Log(LogInfo, fmt.Sprintf("deleted %d value(s)", n))
		return Output{"main": {{"cleared": n}}}, nil
	case "", "get", "set", "increment", "delete":
	default:
		return nil, fmt.Errorf("unknown operation %q", op)
	}

	field, _ := ec.Raw("resultField").(string)
	if field = strings.TrimSpace(field); field == "" {
		field = "value"
	}
	out := make([]Item, 0, len(ec.Items))
	for i, item := range ec.Items {
		key, err := ec.String("key", item, i)
		if err != nil {
			return nil, err
		}
		if key = strings.TrimSpace(key); key == "" {
			return nil, fmt.Errorf("item %d: key is empty", i)
		}
		var result any
		switch op {
		case "", "get":
			v, ok, err := ec.State.Get(ctx, dataScope, key)
			if err != nil {
				return nil, err
			}
			if !ok {
				if v, err = ec.Param("defaultValue", item, i); err != nil {
					return nil, err
				}
			}
			result = v
		case "set":
			v, err := ec.Param("value", item, i)
			if err != nil {
				return nil, err
			}
			ttl, err := ttlParam(ec, "ttlDays", item, i)
			if err != nil {
				return nil, err
			}
			if err := ec.State.Set(ctx, dataScope, key, v, ttl); err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			result = v
		case "increment":
			by, err := ec.Number("by", item, i)
			if err != nil {
				return nil, err
			}
			ttl, err := ttlParam(ec, "ttlDays", item, i)
			if err != nil {
				return nil, err
			}
			if result, err = ec.State.Increment(ctx, dataScope, key, by, ttl); err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
		case "delete":
			if result, err = ec.State.Delete(ctx, dataScope, key); err != nil {
				return nil, err
			}
		}
		next := make(Item, len(item)+1)
		maps.Copy(next, item)
		next[field] = result
		out = append(out, next)
	}
	return Output{"main": out}, nil
}

func first(items []Item) Item {
	if len(items) > 0 {
		return items[0]
	}
	return nil
}

// --- Remove Duplicates ------------------------------------------------------

type dedupeNode struct{}

func (dedupeNode) Definition() Definition {
	return Definition{
		Type: "data.removeDuplicates", Name: "Remove Duplicates", Category: "Data", Source: "builtin", Module: BuiltinModule,
		Description: "Let each item through only once: within this run, or across all runs (e.g. process every e-mail or " +
			"order exactly once). Repeated items go to the 'duplicates' output.",
		Color: "#0d9488", Inputs: []string{"main"}, Outputs: []string{"main", "duplicates"},
		Params: []Param{
			{Name: "key", Label: "Compare by", Required: true, Default: "{{ json.id }}",
				Description: "Items with the same value are duplicates. {{ json }} compares whole items."},
			{Name: "scope", Label: "Remember", Type: ParamSelect, Default: "runs", NoExpression: true, Options: []Option{
				{"Across runs (items seen in earlier runs are duplicates)", "runs"},
				{"Within this run only", "run"},
			}},
			{Name: "ttlDays", Label: "Forget after (days)", Type: ParamNumber, Default: 30, NoExpression: true,
				ShowIf: map[string][]string{"scope": {"runs"}}, Description: "0 remembers forever."},
			{Name: "history", Label: "History name", NoExpression: true, ShowIf: map[string][]string{"scope": {"runs"}},
				Description: "Optional. Nodes with the same name share their history; by default each node has its own."},
		},
		OutputSchema: passthroughSchema,
	}
}

func (dedupeNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	out := Output{"main": {}, "duplicates": {}}
	keys := make([]string, len(ec.Items))
	for i, item := range ec.Items {
		v, err := ec.Param("key", item, i)
		if err != nil {
			return nil, err
		}
		s := expression.Stringify(v)
		if s == "" {
			return nil, fmt.Errorf("item %d: 'Compare by' is empty", i)
		}
		keys[i] = StateKey(s)
	}

	// Duplicates within this batch.
	firstSeen := make([]bool, len(keys))
	inBatch := map[string]bool{}
	for i, k := range keys {
		firstSeen[i] = !inBatch[k]
		inBatch[k] = true
	}

	fresh := firstSeen
	if scope, _ := ec.Raw("scope").(string); scope != "run" {
		if ec.State == nil {
			return nil, ErrNoState
		}
		history, _ := ec.Raw("history").(string)
		if history = strings.TrimSpace(history); history == "" {
			history = ec.Node.ID
		}
		stateScope := "dedupe:" + history
		var unique []string
		for i, k := range keys {
			if firstSeen[i] {
				unique = append(unique, k)
			}
		}
		var res []bool
		var err error
		if ec.IsTestRun() {
			// Test runs show what would pass, without changing the history.
			res, err = ec.State.Seen(ctx, stateScope, unique)
			for i := range res {
				res[i] = !res[i]
			}
			ec.Log(LogInfo, "test run: the history was not updated")
		} else {
			ttl, terr := ttlParam(ec, "ttlDays", nil, 0)
			if terr != nil {
				return nil, terr
			}
			res, err = ec.State.Remember(ctx, stateScope, unique, ttl)
		}
		if err != nil {
			return nil, err
		}
		isNew := map[string]bool{}
		for j, k := range unique {
			isNew[k] = res[j]
		}
		fresh = make([]bool, len(keys))
		for i, k := range keys {
			fresh[i] = firstSeen[i] && isNew[k]
		}
	}
	for i, item := range ec.Items {
		if fresh[i] {
			out["main"] = append(out["main"], item)
		} else {
			out["duplicates"] = append(out["duplicates"], item)
		}
	}
	return out, nil
}

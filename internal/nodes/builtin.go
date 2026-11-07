package nodes

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/wolfposd/g8g/internal/expression"
)

// RegisterBuiltins adds all nodes implemented in Go.
func RegisterBuiltins(r *Registry) error {
	for _, n := range []Node{
		manualTrigger{},
		scheduleTrigger{},
		webhookTrigger{},
		httpNode{},
		setNode{},
		ifNode{},
		transformNode{},
		logNode{},
		gateNode{gateAnd},
		gateNode{gateOr},
		gateNode{gateXor},
	} {
		if err := r.Register(n); err != nil {
			return err
		}
	}
	return nil
}

func passthrough(ec *ExecContext) (Output, error) {
	return Output{"main": ec.Items}, nil
}

// --- Manual trigger ---------------------------------------------------------

type manualTrigger struct{}

func (manualTrigger) Definition() Definition {
	return Definition{
		Type: "trigger.manual", Name: "Manual Trigger", Category: "Triggers", Source: "builtin", Module: BuiltinModule,
		Description: "Start the workflow by clicking Run in the editor or via the API.",
		Color:       "#6366f1", Trigger: TriggerManual, Inputs: []string{}, Outputs: []string{"main"},
		Params: []Param{{
			Name: "payload", Label: "Test payload", Type: ParamJSON, NoExpression: true,
			Description: "Default item emitted when run from the editor without a payload.",
			Default:     map[string]any{},
		}},
	}
}

func (manualTrigger) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	m, err := toMap(ec.Raw("payload"))
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	if m == nil {
		m = Item{}
	}
	return Output{"main": {m}}, nil
}

// --- HTTP Request -----------------------------------------------------------

type httpNode struct{}

func (httpNode) Definition() Definition {
	return Definition{
		Type: "core.http", Name: "HTTP Request", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Call any HTTP API. Runs once per input item.",
		Color:       "#0ea5e9", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "method", Label: "Method", Type: ParamSelect, Default: "GET", Options: []Option{
				{"GET", "GET"}, {"POST", "POST"}, {"PUT", "PUT"}, {"PATCH", "PATCH"}, {"DELETE", "DELETE"},
			}},
			{Name: "url", Label: "URL", Type: ParamString, Required: true, Placeholder: "https://api.example.com/items/{{ json.id }}"},
			{Name: "headers", Label: "Headers", Type: ParamJSON, Default: map[string]any{}},
			{Name: "query", Label: "Query parameters", Type: ParamJSON, Default: map[string]any{}},
			{Name: "body", Label: "Body", Type: ParamJSON, Description: "JSON object or raw string."},
			{Name: "timeoutSeconds", Label: "Timeout (s)", Type: ParamNumber, Default: 30},
			{Name: "failOnError", Label: "Fail on HTTP status >= 400", Type: ParamBoolean, Default: true},
		},
	}
}

func (httpNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	out := make([]Item, 0, len(ec.Items))
	for i, item := range ec.Items {
		method, err := ec.String("method", item, i)
		if err != nil {
			return nil, err
		}
		url, err := ec.String("url", item, i)
		if err != nil {
			return nil, err
		}
		params := map[string]map[string]any{}
		for _, name := range []string{"headers", "query"} {
			v, err := ec.Param(name, item, i)
			if err != nil {
				return nil, err
			}
			if params[name], err = toMap(v); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		body, err := ec.Param("body", item, i)
		if err != nil {
			return nil, err
		}
		timeout, err := ec.Number("timeoutSeconds", item, i)
		if err != nil {
			return nil, err
		}
		failOnError, err := ec.Bool("failOnError", item, i)
		if err != nil {
			return nil, err
		}

		res, err := doHTTP(ctx, httpRequest{
			Method: method, URL: url, Headers: params["headers"], Query: params["query"],
			Body: body, Timeout: time.Duration(timeout * float64(time.Second)),
		})
		if err != nil {
			return nil, err
		}
		if status := res["status"].(int); failOnError && status >= 400 {
			return nil, fmt.Errorf("%s %s returned HTTP %d: %s", method, url, status, truncate(expression.Stringify(res["body"]), 300))
		}
		out = append(out, res)
	}
	return Output{"main": out}, nil
}

// --- Set fields -------------------------------------------------------------

type setNode struct{}

func (setNode) Definition() Definition {
	return Definition{
		Type: "core.set", Name: "Set Fields", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Add or overwrite fields on each item. Values may use {{ expressions }}.",
		Color:       "#10b981", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "fields", Label: "Fields", Type: ParamJSON, Required: true,
				Default: map[string]any{"greeting": "Hello {{ json.name ?? 'world' }}"}},
			{Name: "keepExisting", Label: "Keep existing fields", Type: ParamBoolean, Default: true},
		},
	}
}

func (setNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	out := make([]Item, 0, len(ec.Items))
	for i, item := range ec.Items {
		v, err := ec.Param("fields", item, i)
		if err != nil {
			return nil, err
		}
		fields, err := toMap(v)
		if err != nil {
			return nil, fmt.Errorf("fields: %w", err)
		}
		keep, err := ec.Bool("keepExisting", item, i)
		if err != nil {
			return nil, err
		}
		next := Item{}
		if keep {
			maps.Copy(next, item)
		}
		maps.Copy(next, fields)
		out = append(out, next)
	}
	return Output{"main": out}, nil
}

// --- If ---------------------------------------------------------------------

type ifNode struct{}

func (ifNode) Definition() Definition {
	return Definition{
		Type: "core.if", Name: "If", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Route each item to the true or false output.",
		Color:       "#f59e0b", Inputs: []string{"main"}, Outputs: []string{"true", "false"},
		Params: []Param{{
			Name: "condition", Label: "Condition", Type: ParamString, Required: true,
			Placeholder: "{{ json.status == 200 }}", Default: "{{ true }}",
		}},
	}
}

func (ifNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	out := Output{"true": {}, "false": {}}
	for i, item := range ec.Items {
		ok, err := ec.Bool("condition", item, i)
		if err != nil {
			return nil, err
		}
		if ok {
			out["true"] = append(out["true"], item)
		} else {
			out["false"] = append(out["false"], item)
		}
	}
	return out, nil
}

// --- Transform --------------------------------------------------------------

type transformNode struct{}

func (transformNode) Definition() Definition {
	return Definition{
		Type: "core.transform", Name: "Transform Items", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Evaluate one expression over all items (variable `items`) and emit the result. " +
			"Arrays become multiple items, e.g. {{ filter(items, .active) }} or {{ map(items, ({name: .login})) }} (wrap object literals in parentheses inside map/filter).",
		Color: "#8b5cf6", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{{Name: "expression", Label: "Expression", Type: ParamText, Required: true, Default: "{{ items }}"}},
	}
}

func (transformNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	v, err := ec.Param("expression", nil, 0)
	if err != nil {
		return nil, err
	}
	return Output{"main": ToItems(v)}, nil
}

// ToItems converts an arbitrary value into a list of items.
func ToItems(v any) []Item {
	switch t := v.(type) {
	case nil:
		return []Item{}
	case []Item:
		return t
	case map[string]any:
		return []Item{t}
	case []any:
		out := make([]Item, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			} else {
				out = append(out, Item{"value": e})
			}
		}
		return out
	}
	return []Item{{"value": v}}
}

// --- Log --------------------------------------------------------------------

type logNode struct{}

func (logNode) Definition() Definition {
	return Definition{
		Type: "core.log", Name: "Log", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Write a message to the server log and pass items through unchanged.",
		Color:       "#64748b", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{{Name: "message", Label: "Message", Type: ParamString, Default: "{{ json }}"}},
	}
}

func (logNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	for i, item := range ec.Items {
		msg, err := ec.String("message", item, i)
		if err != nil {
			return nil, err
		}
		ec.Logger.Info("workflow log", "node", ec.Node.Name, "message", msg)
	}
	return passthrough(ec)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

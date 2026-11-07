package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/workflow"
)

const (
	// TriggerCall starts a workflow from another workflow (Execute Workflow node).
	TriggerCall TriggerKind = "call"
	// TriggerError starts a workflow when a watched workflow fails.
	TriggerError TriggerKind = "error"
)

// Runtime gives nodes access to engine features. It is nil in unit tests.
type Runtime interface {
	// RunWorkflow runs another workflow of the same organization synchronously
	// from its "Called by Workflow" trigger and returns its last node's output.
	RunWorkflow(ctx context.Context, workflowID string, items []Item) ([]Item, error)
	// ResumeURL is the URL that resumes this execution while it waits for a webhook.
	ResumeURL() string
}

// Wait modes.
const (
	WaitDuration = "duration"
	WaitUntil    = "until"
	WaitWebhook  = "webhook"
)

// WaitRequest is returned as an error by nodes that pause the execution.
// The engine persists the execution and resumes it later.
type WaitRequest struct {
	Mode  string
	Until time.Time // zero: no time limit (webhook mode only)
}

func (w *WaitRequest) Error() string { return "execution paused (" + w.Mode + ")" }

// --- Wait -------------------------------------------------------------------

type waitNode struct{}

func (waitNode) Definition() Definition {
	return Definition{
		Type: "core.wait", Name: "Wait", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Pause the execution for a while, until a time, or until its resume URL is called " +
			"(e.g. an approval link). Paused executions survive restarts.",
		Color: "#a855f7", Inputs: []string{"main"}, Outputs: []string{"main", "timeout"},
		Params: []Param{
			{Name: "mode", Label: "Resume", Type: ParamSelect, Default: WaitDuration, NoExpression: true, Options: []Option{
				{"After a time interval", WaitDuration}, {"At a specific time", WaitUntil}, {"When the resume URL is called", WaitWebhook},
			}},
			{Name: "amount", Label: "Amount", Type: ParamNumber, Default: 5, NoExpression: true,
				ShowIf: map[string][]string{"mode": {WaitDuration}}},
			{Name: "unit", Label: "Unit", Type: ParamSelect, Default: "minutes", NoExpression: true,
				ShowIf: map[string][]string{"mode": {WaitDuration}}, Options: []Option{
					{"Seconds", "seconds"}, {"Minutes", "minutes"}, {"Hours", "hours"}, {"Days", "days"},
				}},
			{Name: "until", Label: "Resume at (local time)", Type: ParamDateTime, NoExpression: true,
				ShowIf: map[string][]string{"mode": {WaitUntil}}},
			{Name: "timezone", Label: "Timezone", Type: ParamString, Default: "UTC", NoExpression: true,
				ShowIf: map[string][]string{"mode": {WaitUntil}}},
			{Name: "timeoutHours", Label: "Give up after (hours)", Type: ParamNumber, Default: 24, NoExpression: true,
				ShowIf:      map[string][]string{"mode": {WaitWebhook}},
				Description: "Then the input items continue on the 'timeout' output. 0 waits forever."},
		},
	}
}

var waitUnits = map[string]time.Duration{
	"seconds": time.Second, "minutes": time.Minute, "hours": time.Hour, "days": 24 * time.Hour,
}

func (w waitNode) Validate(n *workflow.Node) error {
	_, err := w.request(n.Params, time.Now())
	return err
}

func (w waitNode) request(params map[string]any, now time.Time) (*WaitRequest, error) {
	str := func(k, def string) string {
		if s, ok := params[k].(string); ok && s != "" {
			return s
		}
		return def
	}
	num := func(k string, def float64) float64 {
		switch v := params[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
		return def
	}
	switch mode := str("mode", WaitDuration); mode {
	case WaitDuration:
		unit, ok := waitUnits[str("unit", "minutes")]
		if !ok {
			return nil, fmt.Errorf("invalid unit")
		}
		amount := num("amount", 5)
		if amount < 0 {
			return nil, errors.New("amount must not be negative")
		}
		return &WaitRequest{Mode: mode, Until: now.Add(time.Duration(amount * float64(unit)))}, nil
	case WaitUntil:
		loc, err := time.LoadLocation(str("timezone", "UTC"))
		if err != nil {
			return nil, fmt.Errorf("unknown timezone")
		}
		t, err := time.ParseInLocation("2006-01-02T15:04", strings.TrimSpace(str("until", "")), loc)
		if err != nil {
			return nil, errors.New("'Resume at' must look like 2026-12-24T18:00")
		}
		return &WaitRequest{Mode: mode, Until: t}, nil
	case WaitWebhook:
		r := &WaitRequest{Mode: mode}
		if h := num("timeoutHours", 24); h > 0 {
			r.Until = now.Add(time.Duration(h * float64(time.Hour)))
		}
		return r, nil
	default:
		return nil, fmt.Errorf("invalid mode %q", mode)
	}
}

func (w waitNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	r, err := w.request(ec.Node.Params, time.Now())
	if err != nil {
		return nil, err
	}
	return nil, r
}

// --- Execute Workflow -------------------------------------------------------

type executeWorkflowNode struct{}

func (executeWorkflowNode) Definition() Definition {
	return Definition{
		Type: "flow.executeWorkflow", Name: "Execute Workflow", Category: "Flow", Source: "builtin", Module: BuiltinModule,
		Description: "Run another workflow (it needs a 'Called by Workflow' trigger) and continue with its result. " +
			"Run it once, per item, or per batch with a delay to loop over many items without hitting rate limits.",
		Color: "#0891b2", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "workflow", Label: "Workflow", Type: ParamWorkflow, Required: true, NoExpression: true},
			{Name: "mode", Label: "Run", Type: ParamSelect, Default: "once", NoExpression: true, Options: []Option{
				{"Once with all items", "once"}, {"Once per item (loop)", "each"}, {"Per batch of items (loop)", "batch"},
			}},
			{Name: "batchSize", Label: "Batch size", Type: ParamNumber, Default: 10, NoExpression: true,
				ShowIf: map[string][]string{"mode": {"batch"}}},
			{Name: "delaySeconds", Label: "Pause between runs (s)", Type: ParamNumber, Default: 0, NoExpression: true,
				ShowIf: map[string][]string{"mode": {"each", "batch"}}},
		},
	}
}

func (executeWorkflowNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	if ec.Runtime == nil {
		return nil, errors.New("sub-workflows are not available")
	}
	id, _ := ec.Raw("workflow").(string)
	if id == "" {
		return nil, errors.New("no workflow selected")
	}
	mode, _ := ec.Raw("mode").(string)
	size := 1
	switch mode {
	case "", "once":
		size = max(len(ec.Items), 1)
	case "batch":
		if n, _ := ec.Number("batchSize", nil, 0); n >= 1 {
			size = int(n)
		}
	}
	delay, _ := ec.Number("delaySeconds", nil, 0)

	out := []Item{}
	for start := 0; start < len(ec.Items) || (start == 0 && len(ec.Items) == 0); start += size {
		if start > 0 && delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(delay * float64(time.Second))):
			}
		}
		batch := ec.Items[start:min(start+size, len(ec.Items))]
		res, err := ec.Runtime.RunWorkflow(ctx, id, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, res...)
		if len(ec.Items) == 0 {
			break
		}
	}
	return Output{"main": out}, nil
}

// --- Called by Workflow trigger ---------------------------------------------

type callTrigger struct{}

func (callTrigger) Definition() Definition {
	return Definition{
		Type: "trigger.call", Name: "Called by Workflow", Category: "Triggers", Source: "builtin", Module: BuiltinModule,
		Description: "Start this workflow from an 'Execute Workflow' node of another workflow. " +
			"It receives the caller's items; the output of the last node is returned to the caller.",
		Color: "#0891b2", Trigger: TriggerCall, Inputs: []string{}, Outputs: []string{"main"},
		Params: []Param{{
			Name: "payload", Label: "Test items", Type: ParamJSON, NoExpression: true, Default: []any{map[string]any{}},
			Description: "Items used when the workflow is run manually from the editor.",
		}},
	}
}

func (callTrigger) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	return Output{"main": ToItems(ec.Raw("payload"))}, nil
}

// --- Error trigger ----------------------------------------------------------

type errorTrigger struct{}

func (errorTrigger) Definition() Definition {
	return Definition{
		Type: "trigger.error", Name: "Error Trigger", Category: "Triggers", Source: "builtin", Module: BuiltinModule,
		Description: "Start when a workflow fails: this workflow, or selected workflows (this one must then be active). " +
			"Emits the failed workflow, execution, node and error message.",
		Color: "#dc2626", Trigger: TriggerError, Inputs: []string{}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "scope", Label: "Watch", Type: ParamSelect, Default: "self", NoExpression: true, Options: []Option{
				{"This workflow", "self"}, {"Selected workflows", "selected"},
			}},
			{Name: "workflows", Label: "Workflows", Type: ParamWorkflows, NoExpression: true,
				ShowIf: map[string][]string{"scope": {"selected"}}},
			{Name: "includeManual", Label: "Also for manual test runs", Type: ParamBoolean, Default: false, NoExpression: true},
		},
	}
}

func (errorTrigger) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	// Manual run from the editor: emit a sample.
	return Output{"main": {{
		"workflow":   map[string]any{"id": "sample", "name": "Sample workflow"},
		"execution":  map[string]any{"id": "sample", "triggerType": "schedule"},
		"failedNode": "HTTP Request",
		"error":      "node \"HTTP Request\": GET https://example.com returned HTTP 500",
		"test":       true,
	}}}, nil
}

// ErrorTriggerWatches reports whether an error trigger with params fires for a
// failure of workflow failedID in workflow ownerID.
func ErrorTriggerWatches(params map[string]any, ownerID, failedID, triggerType string) bool {
	if triggerType == string(TriggerError) {
		return false // never react to failures of error handlers (loops)
	}
	if triggerType == string(TriggerManual) && params["includeManual"] != true {
		return false
	}
	scope, _ := params["scope"].(string)
	if scope == "" || scope == "self" {
		return ownerID == failedID
	}
	for _, id := range ParseList(params["workflows"]) {
		if id == failedID {
			return true
		}
	}
	return false
}

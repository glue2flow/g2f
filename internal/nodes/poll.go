package nodes

import (
	"context"
	"fmt"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
)

// TriggerPoll triggers check a source periodically (e.g. a mailbox) and start
// a run only when they find new items.
const TriggerPoll TriggerKind = "poll"

// PollParam is the interval parameter every polling trigger has.
const PollParam = "pollMinutes"

const (
	defaultPollInterval = 5 * time.Minute
	minPollInterval     = time.Minute
	// pollKeyTTL: how long a polling trigger remembers items it has emitted.
	pollKeyTTL = 90 * day
)

func pollIntervalParam() Param {
	return Param{
		Name: PollParam, Label: "Check every (minutes)", Type: ParamNumber, Default: 5, NoExpression: true,
		Description: "While the workflow is active. The first check only remembers what is already there; " +
			"later checks start a run with the new items. ▶ Run shows the current items without remembering them.",
	}
}

// PollInterval reads the interval of a polling trigger node.
func PollInterval(params map[string]any) time.Duration {
	var m float64
	switch v := params[PollParam].(type) {
	case float64:
		m = v
	case int:
		m = float64(v)
	}
	if m <= 0 {
		return defaultPollInterval
	}
	return max(time.Duration(m*float64(time.Minute)), minPollInterval)
}

// pollTrigger adapts a check function to a trigger node: when the engine
// starts a run with the items a check found, the node passes them on;
// without items (manual run) it checks in test mode.
func pollTrigger(ctx context.Context, ec *ExecContext, check func(context.Context, *ExecContext) ([]Item, error)) (Output, error) {
	if len(ec.Items) > 0 {
		return passthrough(ec)
	}
	items, err := check(ctx, ec)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []Item{}
	}
	return Output{"main": items}, nil
}

const pollInitKey = "__initialized"

// PollNew keeps the items a polling trigger has not emitted before. The key
// of an item is the expression keyExpr (e.g. {{ json.id }}). The first check
// after the trigger was set up only remembers the current items, so existing
// mail or records do not start runs. Test runs return all items and record
// nothing.
func PollNew(ctx context.Context, ec *ExecContext, items []Item, keyExpr string) ([]Item, error) {
	if ec.IsTestRun() {
		return items, nil
	}
	if ec.State == nil {
		return nil, ErrNoState
	}
	scope := "poll:" + ec.Node.ID
	keys := make([]string, len(items))
	for i, item := range items {
		v, err := expression.Render(keyExpr, ec.ItemEnv(item, i))
		if err != nil {
			return nil, fmt.Errorf("poll key: %w", err)
		}
		s := expression.Stringify(v)
		if s == "" {
			return nil, fmt.Errorf("poll key %s is empty for item %d", keyExpr, i)
		}
		keys[i] = StateKey(s)
	}
	_, initialized, err := ec.State.Get(ctx, scope, pollInitKey)
	if err != nil {
		return nil, err
	}
	fresh, err := ec.State.Remember(ctx, scope, keys, pollKeyTTL)
	if err != nil {
		return nil, err
	}
	if !initialized {
		if err := ec.State.Set(ctx, scope, pollInitKey, true, 0); err != nil {
			return nil, err
		}
		ec.Log(LogInfo, fmt.Sprintf("first check: remembered %d existing item(s)", len(items)))
		return nil, nil
	}
	var out []Item
	for i, item := range items {
		if fresh[i] {
			out = append(out, item)
		}
	}
	return out, nil
}

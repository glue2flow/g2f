package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// CodeNodeEnabled can be switched off by administrators (G2F_DISABLE_CODE_NODE).
var CodeNodeEnabled = true

const (
	defaultCodeTimeout = 10 * time.Second
	maxCodeLogs        = 100
)

// codeNode runs JavaScript (ES5.1+ with many ES6 features) in an embedded,
// sandboxed interpreter: no file system, network, timers or modules. Each run
// gets a fresh VM, data is passed in as deep copies, and runs are interrupted
// after the timeout.
type codeNode struct{}

func (codeNode) Definition() Definition {
	return Definition{
		Type: "core.code", Name: "Code", Category: "Core", Source: "builtin", Module: BuiltinModule,
		Description: "Transform items with JavaScript. Sandboxed: no network, files or modules.",
		Color:       "#eab308", Inputs: []string{"main"}, Outputs: []string{"main"},
		Params: []Param{
			{Name: "mode", Label: "Run", Type: ParamSelect, Default: "all", NoExpression: true, Options: []Option{
				{"Once for all items", "all"}, {"Once per item", "each"},
			}},
			{Name: "code", Label: "JavaScript", Type: ParamCode, NoExpression: true, Required: true,
				Default: "// `items` is the array of input items. Return an array of objects.\n" +
					"return items.map(item => ({ ...item, processed: true }));",
				Description: "Mode 'all': use `items`, return an array. Mode 'each': use `item` and `index`, return an object " +
					"(or null to drop the item). Also available: nodes, trigger, workflow, execution, console.log()."},
			{Name: "timeoutSeconds", Label: "Timeout (s)", Type: ParamNumber, Default: 10, NoExpression: true},
		},
	}
}

func (codeNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	if !CodeNodeEnabled {
		return nil, errors.New("the Code node is disabled on this server")
	}
	code, _ := ec.Raw("code").(string)
	mode, _ := ec.Raw("mode").(string)
	timeout := defaultCodeTimeout
	if t, err := ec.Number("timeoutSeconds", nil, 0); err == nil && t > 0 {
		timeout = time.Duration(t * float64(time.Second))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	stop := context.AfterFunc(ctx, func() { vm.Interrupt("timeout") })
	defer stop()

	logs := 0
	console := vm.NewObject()
	for name, level := range map[string]string{
		"log": LogInfo, "info": LogInfo, "debug": LogDebug, "warn": LogWarn, "error": LogError,
	} {
		_ = console.Set(name, func(call goja.FunctionCall) goja.Value {
			if logs < maxCodeLogs {
				parts := make([]string, len(call.Arguments))
				for i, a := range call.Arguments {
					parts[i] = jsString(a)
				}
				ec.Log(level, strings.Join(parts, " "))
				logs++
			}
			return goja.Undefined()
		})
	}
	_ = vm.Set("console", console)
	for _, name := range []string{"nodes", "trigger", "workflow", "execution"} {
		if err := setCopy(vm, name, ec.Env[name]); err != nil {
			return nil, err
		}
	}

	var out []Item
	var err error
	switch mode {
	case "each":
		out, err = runEach(vm, code, ec.Items)
	default:
		out, err = runAll(vm, code, ec.Items)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("code timed out after %s", timeout)
		}
		return nil, err
	}
	return Output{"main": out}, nil
}

func runAll(vm *goja.Runtime, code string, items []Item) ([]Item, error) {
	if err := setCopy(vm, "items", items); err != nil {
		return nil, err
	}
	v, err := vm.RunString("(function(){\n" + code + "\n})()")
	if err != nil {
		return nil, jsError(err)
	}
	return exportItems(v)
}

func runEach(vm *goja.Runtime, code string, items []Item) ([]Item, error) {
	fnVal, err := vm.RunString("(function(item, index){\n" + code + "\n})")
	if err != nil {
		return nil, jsError(err)
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, errors.New("internal: code is not a function")
	}
	out := []Item{}
	for i, item := range items {
		arg, err := copyValue(vm, item)
		if err != nil {
			return nil, err
		}
		v, err := fn(goja.Undefined(), arg, vm.ToValue(i))
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, jsError(err))
		}
		res, err := exportItems(v)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, res...)
	}
	return out, nil
}

// setCopy exposes a deep copy of v, so scripts cannot modify data shared with
// other nodes or stored executions.
func setCopy(vm *goja.Runtime, name string, v any) error {
	val, err := copyValue(vm, v)
	if err != nil {
		return err
	}
	return vm.Set(name, val)
}

func copyValue(vm *goja.Runtime, v any) (goja.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	return vm.ToValue(parsed), nil
}

// exportItems converts a script result to items: arrays become several items,
// objects one item, null/undefined none and other values {"value": ...}.
func exportItems(v goja.Value) ([]Item, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return []Item{}, nil
	}
	b, err := json.Marshal(v.Export())
	if err != nil {
		return nil, fmt.Errorf("result is not JSON-serializable: %w", err)
	}
	var parsed any
	if err := json.Unmarshal(b, &parsed); err != nil {
		return nil, err
	}
	return ToItems(parsed), nil
}

func jsError(err error) error {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return errors.New(ex.Error())
	}
	var intr *goja.InterruptedError
	if errors.As(err, &intr) {
		return errors.New("interrupted")
	}
	return err
}

func jsString(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if obj, ok := v.(*goja.Object); ok && obj.ClassName() != "Function" {
		if b, err := json.Marshal(obj.Export()); err == nil {
			return string(b)
		}
	}
	return v.String()
}

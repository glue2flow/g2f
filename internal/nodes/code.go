package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// CodeNodeEnabled can be switched off by administrators (G2F_DISABLE_CODE_NODE).
var CodeNodeEnabled = true

// CodeMemoryLimit is the memory a Code node script may use (G2F_CODE_MEMORY_MB).
// Scripts then run in a separate sandbox process (see codesandbox.go). 0 runs
// them inside the server without a limit.
var CodeMemoryLimit int64 = 128 << 20

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
		OutputSchema: dynamicSchema,
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
	req := scriptRequest{
		Code: code, Mode: mode, Items: ec.Items, TimeoutMs: timeout.Milliseconds(),
		Env: map[string]any{}, MemoryBytes: CodeMemoryLimit,
	}
	for _, name := range []string{"nodes", "trigger", "workflow", "execution"} {
		req.Env[name] = ec.Env[name]
	}

	var res *scriptResult
	var err error
	if CodeMemoryLimit > 0 {
		res, err = runSandboxed(ctx, &req)
	} else {
		res = runScript(ctx, &req)
	}
	if err != nil {
		return nil, err
	}
	for _, l := range res.Logs {
		ec.Log(l.Level, l.Message)
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	return Output{"main": ToItems(res.Items)}, nil
}

// scriptRequest is everything a script run needs. It is JSON so it can be
// handed to a sandbox process.
type scriptRequest struct {
	Code        string         `json:"code"`
	Mode        string         `json:"mode"`
	Items       []Item         `json:"items"`
	Env         map[string]any `json:"env"`
	TimeoutMs   int64          `json:"timeoutMs"`
	MemoryBytes int64          `json:"memoryBytes"`
}

type scriptResult struct {
	Items any         `json:"items"`
	Logs  []scriptLog `json:"logs,omitempty"`
	Error string      `json:"error,omitempty"`
}

type scriptLog struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// runScript runs a script in a fresh VM. interrupt, if set, receives a
// function that stops the script with the given reason (memory watchdog).
func runScript(ctx context.Context, req *scriptRequest, interrupt ...*atomic.Pointer[func(reason string)]) *scriptResult {
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultCodeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	var reason atomic.Value
	stopVM := func(why string) {
		reason.CompareAndSwap(nil, why)
		vm.Interrupt(why)
	}
	for _, p := range interrupt {
		p.Store(&stopVM)
	}
	stop := context.AfterFunc(ctx, func() { stopVM("timeout") })
	defer stop()

	res := &scriptResult{}
	console := vm.NewObject()
	for name, level := range map[string]string{
		"log": LogInfo, "info": LogInfo, "debug": LogDebug, "warn": LogWarn, "error": LogError,
	} {
		_ = console.Set(name, func(call goja.FunctionCall) goja.Value {
			if len(res.Logs) < maxCodeLogs {
				parts := make([]string, len(call.Arguments))
				for i, a := range call.Arguments {
					parts[i] = jsString(a)
				}
				res.Logs = append(res.Logs, scriptLog{level, strings.Join(parts, " ")})
			}
			return goja.Undefined()
		})
	}
	_ = vm.Set("console", console)
	for _, name := range []string{"nodes", "trigger", "workflow", "execution"} {
		if err := setCopy(vm, name, req.Env[name]); err != nil {
			res.Error = err.Error()
			return res
		}
	}

	var out []Item
	var err error
	switch req.Mode {
	case "each":
		out, err = runEach(vm, req.Code, req.Items)
	default:
		out, err = runAll(vm, req.Code, req.Items)
	}
	if err != nil {
		switch reason.Load() {
		case "timeout":
			res.Error = fmt.Sprintf("code timed out after %s", timeout)
		case "memory":
			res.Error = memoryError(req.MemoryBytes)
		default:
			res.Error = err.Error()
		}
		return res
	}
	res.Items = out
	return res
}

func memoryError(limit int64) string {
	return fmt.Sprintf("code exceeded its memory limit of %d MB", limit>>20)
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

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
)

// The WASM plugin ABI (JSON over Extism input/output):
//
// The host calls the exported function `execute` once per node run with:
//
//	{
//	  "node":   "sendMessage",                 // node type without module prefix
//	  "items":  [{"json": {...}, "params": {...}, "input": "main"}],
//	  "config": {...},                         // plugin-only settings of the organization
//	  "vars":   {...},                         // organization variables
//	  "oauth":  {"accessToken": "...", "tokenType": "Bearer", "instanceUrl": "..."}, // OAuth2 plugins (instanceUrl if the provider has one)
//	  "workflow":  {"id": "...", "name": "..."},
//	  "execution": {"id": "..."}
//	}
//
// "params" are fully resolved per item (expressions evaluated, secrets decrypted).
// The plugin answers with one of:
//
//	{"items": [{...}, ...]}                   // shorthand for the "main" output
//	{"outputs": {"main": [...], "other": [...]}}
//	{"error": "message"}                      // or set an Extism error
type wasmRequest struct {
	Node   string         `json:"node"`
	Items  []wasmItem     `json:"items"`
	Config map[string]any `json:"config"`
	Vars   map[string]any `json:"vars"`
	// OAuth is the connected account's access token (OAuth2 plugins only).
	OAuth     *nodes.OAuthToken `json:"oauth,omitempty"`
	Workflow  any               `json:"workflow,omitempty"`
	Execution any               `json:"execution,omitempty"`
}

type wasmItem struct {
	JSON   nodes.Item     `json:"json"`
	Params map[string]any `json:"params"`
	Input  string         `json:"input"`
}

type wasmResponse struct {
	Items   []nodes.Item            `json:"items"`
	Outputs map[string][]nodes.Item `json:"outputs"`
	Error   string                  `json:"error"`
}

const (
	defaultWasmTimeout  = 30 * time.Second
	defaultWasmMemoryMB = 64
	maxHTTPResponse     = 10 << 20
)

// wasmModule is a compiled plugin shared by its nodes. Every node run gets a
// fresh instance, so plugins cannot keep state between runs or leak data
// between workflows.
type wasmModule struct {
	compiled *extism.CompiledPlugin
	timeout  time.Duration
	hosts    []string // allowedHosts, re-checked on every redirect
	log      *slog.Logger
}

func compileWasm(ctx context.Context, m *nodes.Manifest, wasm []byte, log *slog.Logger) (*wasmModule, error) {
	timeout := defaultWasmTimeout
	if m.Timeout != "" {
		timeout, _ = time.ParseDuration(m.Timeout)
	}
	mem := m.MemoryMB
	if mem <= 0 {
		mem = defaultWasmMemoryMB
	}
	manifest := extism.Manifest{
		Wasm:         []extism.Wasm{extism.WasmData{Data: wasm, Name: "main"}},
		AllowedHosts: m.AllowedHosts, // empty: no outbound HTTP at all
		Timeout:      uint64(timeout.Milliseconds()),
		Memory: &extism.ManifestMemory{
			MaxPages:             uint32(mem * 16), // 64 KiB pages
			MaxHttpResponseBytes: maxHTTPResponse,
		},
	}
	compiled, err := extism.NewCompiledPlugin(ctx, manifest, extism.PluginConfig{EnableWasi: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("compile wasm: %w", err)
	}
	// Fail at import time rather than at the first run.
	inst, err := compiled.Instance(ctx, extism.PluginInstanceConfig{})
	if err != nil {
		compiled.Close(ctx)
		return nil, fmt.Errorf("instantiate wasm: %w", err)
	}
	defer inst.Close(ctx)
	if !inst.FunctionExists("execute") {
		compiled.Close(ctx)
		return nil, fmt.Errorf("wasm module does not export an `execute` function")
	}
	return &wasmModule{compiled: compiled, timeout: timeout, hosts: m.AllowedHosts, log: log.With("module", m.Module)}, nil
}

func (w *wasmModule) close() { _ = w.compiled.Close(context.Background()) }

type wasmNode struct {
	def      nodes.Definition
	nodeType string
	mod      *wasmModule
}

func wasmNodes(m *nodes.Manifest, mod *wasmModule, source string) []nodes.Node {
	out := make([]nodes.Node, 0, len(m.Nodes))
	for _, n := range m.Nodes {
		out = append(out, &wasmNode{def: m.NodeDefinition(n, source), nodeType: n.Type, mod: mod})
	}
	return out
}

func (n *wasmNode) Definition() nodes.Definition { return n.def }

func (n *wasmNode) Execute(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	req := wasmRequest{Node: n.nodeType, Config: ec.Config, Workflow: ec.Env["workflow"], Execution: ec.Env["execution"]}
	req.Vars = ec.Vars
	req.OAuth = ec.OAuth
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	if req.Vars == nil {
		req.Vars = map[string]any{}
	}
	handles := make([]string, 0, len(ec.Inputs))
	for h := range ec.Inputs {
		handles = append(handles, h)
	}
	sort.Strings(handles)
	i := 0
	for _, h := range handles {
		for _, item := range ec.Inputs[h] {
			params, err := nodes.ResolveParams(ec, item, i)
			if err != nil {
				return nil, err
			}
			req.Items = append(req.Items, wasmItem{JSON: item, Params: params, Input: h})
			i++
		}
	}
	input, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	ctx = netguard.WithAllowedHosts(ctx, n.mod.hosts)
	ctx, cancel := context.WithTimeout(ctx, n.mod.timeout)
	defer cancel()
	inst, err := n.mod.compiled.Instance(ctx, extism.PluginInstanceConfig{})
	if err != nil {
		return nil, fmt.Errorf("instantiate plugin: %w", err)
	}
	defer inst.Close(context.Background())
	log := n.mod.log.With("node", ec.Node.Name)
	inst.SetLogger(func(level extism.LogLevel, msg string) {
		log.Info("plugin log", "level", level.String(), "message", msg)
	})

	_, output, err := inst.CallWithContext(ctx, "execute", input)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("plugin timed out after %s", n.mod.timeout)
		}
		return nil, fmt.Errorf("plugin: %s", redactURLs(err.Error()))
	}
	var res wasmResponse
	if err := json.Unmarshal(output, &res); err != nil {
		return nil, fmt.Errorf("plugin returned invalid JSON: %w", err)
	}
	if res.Error != "" {
		return nil, fmt.Errorf("%s", redactURLs(res.Error))
	}
	if res.Outputs != nil {
		return res.Outputs, nil
	}
	if res.Items == nil {
		res.Items = []nodes.Item{}
	}
	return nodes.Output{"main": res.Items}, nil
}

var urlRe = regexp.MustCompile(`(https?://[^/\s'"]+)(/[^\s'"]*)?`)

// redactURLs keeps only scheme and host of URLs in plugin errors: APIs such as
// Telegram's carry credentials in the path.
func redactURLs(s string) string {
	return urlRe.ReplaceAllStringFunc(s, func(u string) string {
		m := urlRe.FindStringSubmatch(u)
		if m[2] == "" || m[2] == "/" {
			return u
		}
		return m[1] + "/…"
	})
}

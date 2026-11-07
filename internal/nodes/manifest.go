package nodes

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest describes a plugin module: a set of nodes, their parameters and the
// module's global configuration. Two runtimes exist:
//
//   - "http" (default): each node maps its parameters onto one HTTP request.
//   - "wasm": nodes are implemented in a WebAssembly module (Extism PDK) that
//     exports an `execute` function. See examples/plugins/telegram.
type Manifest struct {
	Module      string `yaml:"module"`
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
	Color       string `yaml:"color"`
	Runtime     string `yaml:"runtime"` // http | wasm

	// Config declares settings for the plugin itself, configured once per
	// organization by an admin (API tokens, client secrets, base URLs).
	// Values are encrypted at rest. They are available ONLY to the plugin:
	// WASM code receives them as `config`, HTTP manifests can use
	// {{ config.<name> }} in their request templates. Expressions written by
	// workflow editors in node fields cannot read them.
	Config []Param `yaml:"config"`

	// Vars declares organization-wide values that workflow editors may use in
	// node fields as {{ vars.<name> }} (e.g. a default chat or channel).
	// Vars cannot be secrets.
	Vars []Param `yaml:"vars"`

	// Params are per-node parameters shared by all nodes of the module.
	// Parameters of type "secret" are stored encrypted per node instance
	// (e.g. each user's API token).
	Params []Param `yaml:"params"`

	// http runtime
	BaseURL string         `yaml:"baseUrl"`
	Headers map[string]any `yaml:"headers"`

	// wasm runtime
	Wasm         string   `yaml:"wasm"`         // file name inside the bundle (default plugin.wasm)
	AllowedHosts []string `yaml:"allowedHosts"` // hosts the plugin may call over HTTP
	Timeout      string   `yaml:"timeout"`      // per node execution, default 30s
	MemoryMB     int      `yaml:"memoryMB"`     // default 64

	Nodes []ManifestNode `yaml:"nodes"`
}

type ManifestNode struct {
	Type        string   `yaml:"type"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Params      []Param  `yaml:"params"`
	Inputs      []string `yaml:"inputs"`  // default [main]
	Outputs     []string `yaml:"outputs"` // default [main]

	// http runtime
	Request ManifestRequest `yaml:"request"`
	// Output is an optional expression over `response` producing the emitted
	// item(s). Defaults to {{ response.body }}.
	Output string `yaml:"output"`
	// FailOnError fails the node on HTTP status >= 400 (default true).
	FailOnError *bool `yaml:"failOnError"`
}

type ManifestRequest struct {
	Method  string         `yaml:"method"`
	Path    string         `yaml:"path"` // appended to baseUrl
	URL     string         `yaml:"url"`  // absolute URL, overrides baseUrl+path
	Headers map[string]any `yaml:"headers"`
	Query   map[string]any `yaml:"query"`
	Body    any            `yaml:"body"`
	Timeout string         `yaml:"timeout"`
}

var (
	moduleRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,39}$`)
	nameRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
)

// reservedModules cannot be used by plugins because built-in node types use them.
var reservedModules = map[string]bool{BuiltinModule: true, "core": true, "trigger": true, "logic": true}

// ParseManifest decodes and validates a manifest.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if !moduleRe.MatchString(m.Module) {
		return fmt.Errorf("module %q must match %s", m.Module, moduleRe)
	}
	if reservedModules[m.Module] {
		return fmt.Errorf("module name %q is reserved", m.Module)
	}
	if m.Name == "" {
		m.Name = m.Module
	}
	switch m.Runtime {
	case "":
		m.Runtime = "http"
	case "http", "wasm":
	default:
		return fmt.Errorf("unknown runtime %q", m.Runtime)
	}
	if m.Runtime == "wasm" && m.Wasm == "" {
		m.Wasm = "plugin.wasm"
	}
	if m.Timeout != "" {
		if _, err := time.ParseDuration(m.Timeout); err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}
	if len(m.Nodes) == 0 {
		return fmt.Errorf("at least one node is required")
	}
	if err := normalizeParams(m.Config, "config"); err != nil {
		return err
	}
	if err := normalizeParams(m.Vars, "vars"); err != nil {
		return err
	}
	settings := map[string]bool{}
	for _, p := range m.Config {
		settings[p.Name] = true
	}
	for _, p := range m.Vars {
		if p.Type == ParamSecret {
			return fmt.Errorf("vars: %q cannot be a secret; put secrets into config", p.Name)
		}
		if settings[p.Name] {
			return fmt.Errorf("%q is declared in both config and vars", p.Name)
		}
	}
	if err := normalizeParams(m.Params, "params"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		n.Type = strings.TrimPrefix(n.Type, m.Module+".")
		if !nameRe.MatchString(n.Type) || n.Name == "" {
			return fmt.Errorf("node %d: valid 'type' and 'name' are required", i)
		}
		if seen[n.Type] {
			return fmt.Errorf("duplicate node type %q", n.Type)
		}
		seen[n.Type] = true
		if err := normalizeParams(n.Params, n.Type); err != nil {
			return err
		}
		if m.Runtime == "http" && n.Request.URL == "" && m.BaseURL == "" {
			return fmt.Errorf("node %s: request.url or module baseUrl required", n.Type)
		}
		if len(n.Inputs) == 0 {
			n.Inputs = []string{"main"}
		}
		if len(n.Outputs) == 0 {
			n.Outputs = []string{"main"}
		}
	}
	return nil
}

func normalizeParams(ps []Param, where string) error {
	names := map[string]bool{}
	for i := range ps {
		p := &ps[i]
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("%s: invalid parameter name %q", where, p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("%s: duplicate parameter %q", where, p.Name)
		}
		names[p.Name] = true
		if p.Type == "" {
			p.Type = ParamString
		}
		if p.Label == "" {
			p.Label = p.Name
		}
		switch p.Type {
		case ParamString, ParamText, ParamNumber, ParamBoolean, ParamSelect, ParamJSON, ParamSecret, ParamDateTime, ParamCode:
		default:
			return fmt.Errorf("%s: parameter %q has unknown type %q", where, p.Name, p.Type)
		}
	}
	return nil
}

// Info returns the registry description of the module.
func (m *Manifest) Info(source string) ModuleInfo {
	return ModuleInfo{
		Module: m.Module, Name: m.Name, Version: m.Version, Description: m.Description,
		Color: m.Color, Runtime: m.Runtime, Source: source, Config: m.Config, Vars: m.Vars, AllowedHosts: m.AllowedHosts,
	}
}

// NodeDefinition builds the editor definition for one manifest node.
func (m *Manifest) NodeDefinition(n ManifestNode, source string) Definition {
	params := append(append([]Param{}, m.Params...), n.Params...)
	category := m.Name
	return Definition{
		Type: m.Module + "." + n.Type, Name: n.Name, Category: category, Description: n.Description,
		Color: m.Color, Inputs: n.Inputs, Outputs: n.Outputs, Params: params,
		Module: m.Module, Source: source,
	}
}

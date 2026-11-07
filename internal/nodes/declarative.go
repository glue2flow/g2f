package nodes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/wolfposd/g8g/internal/expression"
)

// Manifest is a YAML file describing one API module with one or more nodes.
// Each node maps its parameters onto an HTTP request; no Go code is required.
// See "Writing modules" in README.md for the format.
type Manifest struct {
	Module      string         `yaml:"module"`
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Color       string         `yaml:"color"`
	BaseURL     string         `yaml:"baseUrl"`
	Params      []Param        `yaml:"params"`  // shared by all nodes (e.g. token)
	Headers     map[string]any `yaml:"headers"` // shared headers
	Nodes       []ManifestNode `yaml:"nodes"`
}

type ManifestNode struct {
	Type        string          `yaml:"type"`
	Name        string          `yaml:"name"`
	Description string          `yaml:"description"`
	Params      []Param         `yaml:"params"`
	Request     ManifestRequest `yaml:"request"`
	// Output is an optional expression over `response` that produces the
	// emitted item(s). Defaults to {{ response.body }}.
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

// LoadModules registers all *.yaml / *.yml manifests from dir.
func LoadModules(r *Registry, dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.y*ml"))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return count, err
		}
		var m Manifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return count, fmt.Errorf("%s: %w", f, err)
		}
		if m.Module == "" {
			return count, fmt.Errorf("%s: 'module' is required", f)
		}
		for _, mn := range m.Nodes {
			n, err := newDeclarativeNode(&m, mn, filepath.Base(f))
			if err != nil {
				return count, fmt.Errorf("%s: %w", f, err)
			}
			if err := r.Register(n); err != nil {
				return count, fmt.Errorf("%s: %w", f, err)
			}
			count++
		}
	}
	return count, nil
}

type declarativeNode struct {
	def     Definition
	module  *Manifest
	spec    ManifestNode
	timeout time.Duration
}

func newDeclarativeNode(m *Manifest, n ManifestNode, file string) (*declarativeNode, error) {
	if n.Type == "" || n.Name == "" {
		return nil, fmt.Errorf("node needs 'type' and 'name'")
	}
	typ := n.Type
	if !strings.HasPrefix(typ, m.Module+".") {
		typ = m.Module + "." + typ
	}
	if n.Request.URL == "" && m.BaseURL == "" {
		return nil, fmt.Errorf("node %s: request.url or module baseUrl required", typ)
	}
	params := append(append([]Param{}, m.Params...), n.Params...)
	for i := range params {
		if params[i].Type == "" {
			params[i].Type = ParamString
		}
		if params[i].Label == "" {
			params[i].Label = params[i].Name
		}
	}
	var timeout time.Duration
	if n.Request.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(n.Request.Timeout); err != nil {
			return nil, fmt.Errorf("node %s: timeout: %w", typ, err)
		}
	}
	category := m.Name
	if category == "" {
		category = m.Module
	}
	return &declarativeNode{
		module: m, spec: n, timeout: timeout,
		def: Definition{
			Type: typ, Name: n.Name, Category: category, Description: n.Description,
			Color: m.Color, Inputs: []string{"main"}, Outputs: []string{"main"},
			Params: params, Source: "module:" + file,
		},
	}, nil
}

func (d *declarativeNode) Definition() Definition { return d.def }

func (d *declarativeNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	var out []Item
	for i, item := range ec.Items {
		// Resolve node params first, then expose them as `params` to the request template.
		params := map[string]any{}
		for _, p := range d.def.Params {
			v, err := ec.Param(p.Name, item, i)
			if err != nil {
				return nil, err
			}
			if p.Required && (v == nil || v == "") {
				return nil, fmt.Errorf("parameter %q is required", p.Label)
			}
			params[p.Name] = v
		}
		env := ec.ItemEnv(item, i)
		env["params"] = params

		req := d.spec.Request
		url := req.URL
		if url == "" {
			url = strings.TrimRight(d.module.BaseURL, "/") + "/" + strings.TrimLeft(req.Path, "/")
		}
		resolve := func(v any) (any, error) { return expression.Resolve(v, env) }

		urlV, err := resolve(url)
		if err != nil {
			return nil, err
		}
		headers := map[string]any{}
		for _, h := range []map[string]any{d.module.Headers, req.Headers} {
			for k, v := range h {
				if headers[k], err = resolve(v); err != nil {
					return nil, err
				}
			}
		}
		queryV, err := resolve(anyMap(req.Query))
		if err != nil {
			return nil, err
		}
		bodyV, err := resolve(normalizeYAML(req.Body))
		if err != nil {
			return nil, err
		}
		query, _ := queryV.(map[string]any)

		res, err := doHTTP(ctx, httpRequest{
			Method: req.Method, URL: expression.Stringify(urlV), Headers: headers,
			Query: query, Body: bodyV, Timeout: d.timeout,
		})
		if err != nil {
			return nil, err
		}
		if status := res["status"].(int); status >= 400 && (d.spec.FailOnError == nil || *d.spec.FailOnError) {
			return nil, fmt.Errorf("%s returned HTTP %d: %s", d.def.Name, status, truncate(expression.Stringify(res["body"]), 300))
		}

		outExpr := d.spec.Output
		if outExpr == "" {
			outExpr = "{{ response.body }}"
		}
		env["response"] = res
		v, err := expression.Render(outExpr, env)
		if err != nil {
			return nil, fmt.Errorf("output: %w", err)
		}
		out = append(out, ToItems(v)...)
	}
	if out == nil {
		out = []Item{}
	}
	return Output{"main": out}, nil
}

func anyMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return normalizeYAML(m)
}

// normalizeYAML converts map[any]any (possible in older yaml decoders) and
// nested values into JSON-compatible types.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalizeYAML(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = normalizeYAML(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalizeYAML(e)
		}
		return out
	}
	return v
}

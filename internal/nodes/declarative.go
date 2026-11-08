package nodes

import (
	"context"
	"fmt"
	neturl "net/url"
	"slices"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
)

// HTTPModuleNodes builds the nodes of an "http" runtime manifest: each node maps
// its parameters onto one HTTP request, no code required.
func HTTPModuleNodes(m *Manifest, source string) ([]Node, error) {
	if m.Runtime != "http" {
		return nil, fmt.Errorf("module %s is not an http module", m.Module)
	}
	out := make([]Node, 0, len(m.Nodes))
	for _, n := range m.Nodes {
		var timeout time.Duration
		if n.Request.Timeout != "" {
			var err error
			if timeout, err = time.ParseDuration(n.Request.Timeout); err != nil {
				return nil, fmt.Errorf("node %s: timeout: %w", n.Type, err)
			}
		}
		out = append(out, &declarativeNode{def: m.NodeDefinition(n, source), module: m, spec: n, timeout: timeout})
	}
	return out, nil
}

type declarativeNode struct {
	def     Definition
	module  *Manifest
	spec    ManifestNode
	timeout time.Duration
}

func (d *declarativeNode) Definition() Definition { return d.def }

// ResolveParams evaluates all parameters of the node for one item and checks
// required ones. Shared by the http and wasm runtimes.
func ResolveParams(ec *ExecContext, item Item, index int) (map[string]any, error) {
	params := map[string]any{}
	for _, p := range ec.Def.Params {
		if p.Type == ParamConnection {
			continue // resolved by the engine into ec.OAuth
		}
		v, err := ec.Param(p.Name, item, index)
		if err != nil {
			return nil, err
		}
		if p.Required && (v == nil || v == "") {
			return nil, fmt.Errorf("parameter %q is required", p.Label)
		}
		params[p.Name] = v
	}
	return params, nil
}

func (d *declarativeNode) Execute(ctx context.Context, ec *ExecContext) (Output, error) {
	if d.spec.Trigger != "" {
		return pollTrigger(ctx, ec, func(ctx context.Context, ec *ExecContext) ([]Item, error) {
			items, err := d.requests(ctx, ec, []Item{{}})
			if err != nil {
				return nil, err
			}
			return PollNew(ctx, ec, items, d.spec.PollKey)
		})
	}
	out, err := d.requests(ctx, ec, ec.Items)
	if err != nil {
		return nil, err
	}
	return Output{"main": out}, nil
}

// requests sends the node's request once per item and returns the output items.
func (d *declarativeNode) requests(ctx context.Context, ec *ExecContext, items []Item) ([]Item, error) {
	var out []Item
	for i, item := range items {
		params, err := ResolveParams(ec, item, i)
		if err != nil {
			return nil, err
		}
		// Request templates are written by the plugin author, so unlike node
		// fields they may use the plugin config (tokens, base URLs).
		env := ec.ItemEnv(item, i)
		env["params"] = params
		env["config"] = ec.Config

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
		// The OAuth token only goes to the module's API hosts, so a URL built
		// from node parameters can never send it elsewhere.
		sendToken := false
		if ec.OAuth != nil {
			if !d.tokenHostAllowed(expression.Stringify(urlV)) {
				return nil, fmt.Errorf("refusing to send the %s access token to %s (not in oauth2.apiHosts)", d.module.Name, hostOf(expression.Stringify(urlV)))
			}
			sendToken = true
			env["oauth"] = map[string]any{"accessToken": ec.OAuth.AccessToken, "tokenType": ec.OAuth.TokenType}
		}
		headers := map[string]any{}
		for _, h := range []map[string]any{d.module.Headers, req.Headers} {
			for k, v := range h {
				if headers[k], err = resolve(v); err != nil {
					return nil, err
				}
			}
		}
		if sendToken && !hasHeader(headers, "Authorization") {
			headers["Authorization"] = ec.OAuth.AuthorizationHeader()
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
	return out, nil
}

func (d *declarativeNode) tokenHostAllowed(raw string) bool {
	if d.module.OAuth2 == nil {
		return false
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	return slices.Contains(d.module.OAuth2.APIHosts, u.Hostname())
}

func hostOf(raw string) string {
	if u, err := neturl.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func hasHeader(h map[string]any, name string) bool {
	for k, v := range h {
		if strings.EqualFold(k, name) && expression.Stringify(v) != "" {
			return true
		}
	}
	return false
}

func anyMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return normalizeYAML(m)
}

// normalizeYAML converts map[any]any and nested values into JSON-compatible types.
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

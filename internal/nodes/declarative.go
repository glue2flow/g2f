package nodes

import (
	"context"
	"fmt"
	neturl "net/url"
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
			continue // resolved by the engine into ec.OAuth or ec.Credential
		}
		v, err := ec.Param(p.Name, item, index)
		if err != nil {
			return nil, err
		}
		if p.Required && (v == nil || v == "") {
			return nil, fmt.Errorf("parameter %q is required", p.Label)
		}
		params[p.Name] = plainPaths(v)
	}
	return params, nil
}

// plainPaths turns rawPath(...) values written in node fields (or carried in
// item data) back into plain strings: only the module's own path template may
// opt out of escaping, never the workflow author.
func plainPaths(v any) any {
	switch t := v.(type) {
	case expression.RawPath:
		return string(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = plainPaths(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainPaths(e)
		}
		return out
	}
	return v
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
		resolve := func(v any) (any, error) { return expression.Resolve(v, env) }

		// The oauth scope is for the manifest (e.g. baseUrl: "{{ oauth.instanceUrl }}");
		// node fields never see it.
		if ec.OAuth != nil {
			if d.module.OAuth2 != nil && d.module.OAuth2.InstanceURLField != "" && ec.OAuth.InstanceURL == "" {
				return nil, fmt.Errorf("the %s account has no instance URL; reconnect the account", d.module.Name)
			}
			env["oauth"] = map[string]any{"accessToken": ec.OAuth.AccessToken, "tokenType": ec.OAuth.TokenType, "instanceUrl": ec.OAuth.InstanceURL}
		}
		// Credential values, like the oauth scope, are for the manifest only.
		// Without a selected credential (optional) the field defaults apply.
		if c := d.module.Credentials; c != nil {
			cred := ec.Credential
			if cred == nil {
				cred = c.Defaults()
			}
			env["credential"] = cred
		}
		reqURL, err := d.requestURL(env)
		if err != nil {
			return nil, err
		}
		// Requests of credential modules only go to credentials.apiHosts,
		// whatever node parameters or credential fields say.
		if c := d.module.Credentials; c != nil {
			u, err := neturl.Parse(reqURL)
			if err != nil || u.Host == "" {
				return nil, fmt.Errorf("%s: invalid request URL", d.module.Name)
			}
			if !c.AllowsURL(u) {
				return nil, fmt.Errorf("refusing to send a %s request to %s (not in credentials.apiHosts)", d.module.Name, u.Host)
			}
		}
		// The OAuth token only goes to the module's API hosts (https, default port
		// unless the entry names one), so a URL built
		// from node parameters can never send it elsewhere.
		sendToken := false
		if ec.OAuth != nil {
			if !d.tokenHostAllowed(reqURL) {
				return nil, fmt.Errorf("refusing to send the %s access token to %s (not in oauth2.apiHosts)", d.module.Name, hostOf(reqURL))
			}
			sendToken = true
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
			Method: req.Method, URL: reqURL, Headers: headers,
			Query: query, Body: bodyV, Timeout: d.timeout, SameHostOnly: true,
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

// requestURL renders the request URL: baseUrl plus path, or the url template.
// Every {{ }} block in the path part is percent-encoded as one path segment
// (see pathSegment), so a parameter cannot add slashes, dot segments, a query
// or a fragment. The rest (baseUrl, host, query part) is rendered as written.
func (d *declarativeNode) requestURL(env expression.Env) (string, error) {
	return d.requestURLFor(d.spec.Request, env)
}

func (d *declarativeNode) requestURLFor(req ManifestRequest, env expression.Env) (string, error) {
	if req.URL != "" {
		return expression.RenderWith(req.URL, env, func(prefix string, v any) (string, error) {
			if inURLPath(prefix) {
				return pathSegment(v)
			}
			return expression.Stringify(v), nil
		})
	}
	base, err := expression.Render(d.module.BaseURL, env)
	if err != nil {
		return "", err
	}
	path, err := expression.RenderWith(req.Path, env, func(_ string, v any) (string, error) { return pathSegment(v) })
	if err != nil {
		return "", err
	}
	return strings.TrimRight(expression.Stringify(base), "/") + "/" + strings.TrimLeft(path, "/"), nil
}

// inURLPath reports whether the end of the URL template prefix is in the path:
// after the host's first "/" and before any "?" or "#".
func inURLPath(prefix string) bool {
	_, rest, ok := strings.Cut(prefix, "://")
	if !ok {
		return false
	}
	_, path, ok := strings.Cut(rest, "/")
	return ok && !strings.ContainsAny(path, "?#")
}

// pathSegment renders the value of one {{ }} block of a path. Plain values are
// percent-encoded as one segment and may not be "." or "..". rawPath(x) values
// are inserted as written, but must not hold "?", "#", backslashes, control
// characters or a "." / ".." segment (also not percent-encoded).
func pathSegment(v any) (string, error) {
	if raw, ok := v.(expression.RawPath); ok {
		s := string(raw)
		if strings.ContainsAny(s, "?#\\") || strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return "", fmt.Errorf("path value %q must not contain ?, #, backslashes or control characters", s)
		}
		for _, seg := range strings.Split(s, "/") {
			if dec, err := neturl.PathUnescape(seg); err != nil || dec == "." || dec == ".." {
				return "", fmt.Errorf("path value %q must not contain \".\" or \"..\" segments", s)
			}
		}
		return s, nil
	}
	s := expression.Stringify(v)
	if s == "." || s == ".." {
		return "", fmt.Errorf("path value %q is not allowed", s)
	}
	return neturl.PathEscape(s), nil
}

func (d *declarativeNode) tokenHostAllowed(raw string) bool {
	u, err := neturl.Parse(raw)
	return err == nil && d.module.OAuth2.AllowsURL(u)
}

// allowInsecureAPIHosts lets tests send tokens to plain http servers.
var allowInsecureAPIHosts bool

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

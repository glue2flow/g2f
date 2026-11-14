package nodes

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
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
//   - "go": nodes are implemented in Go and compiled into the server (see
//     RegisterNative), for protocols the sandbox cannot speak (SMTP, IMAP, …).
type Manifest struct {
	Module      string `yaml:"module"`
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
	Color       string `yaml:"color"`
	// Icon is an SVG, PNG or WebP file next to the manifest (e.g.
	// icons/gmail.svg), inline SVG markup, or a base64 data URI. See
	// ResolveIcons; IconURI holds the validated data URI.
	Icon    string `yaml:"icon"`
	IconURI string `yaml:"-"`
	Runtime string `yaml:"runtime"` // http | wasm | go

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

	// OAuth2 lets users connect their own accounts (authorization code flow).
	// The host runs the flow, stores and refreshes tokens, and hands the access
	// token to the plugin; workflow editors never see it.
	OAuth2 *OAuth2Spec `yaml:"oauth2"`

	// wasm runtime
	Wasm         string   `yaml:"wasm"`         // file name inside the bundle (default plugin.wasm)
	AllowedHosts []string `yaml:"allowedHosts"` // hosts the plugin may call over HTTP
	Timeout      string   `yaml:"timeout"`      // per node execution, default 30s
	MemoryMB     int      `yaml:"memoryMB"`     // default 64

	Nodes []ManifestNode `yaml:"nodes"`
}

// OAuth2Spec describes the provider of an OAuth2 plugin.
type OAuth2Spec struct {
	AuthorizeURL    string            `yaml:"authorizeUrl" json:"authorizeUrl"`
	TokenURL        string            `yaml:"tokenUrl" json:"tokenUrl"`
	Scopes          []string          `yaml:"scopes" json:"scopes"`
	ScopeSeparator  string            `yaml:"scopeSeparator" json:"-"`
	AuthorizeParams map[string]string `yaml:"authorizeParams" json:"-"` // e.g. access_type: offline
	PKCE            bool              `yaml:"pkce" json:"pkce"`
	// TokenAuth: how the client authenticates at the token endpoint:
	// "body" (client_id/client_secret form fields, default) or "basic".
	TokenAuth string `yaml:"tokenAuth" json:"-"`
	// Names of the config entries holding the OAuth app credentials.
	ClientIDConfig     string `yaml:"clientIdConfig" json:"clientIdConfig"`
	ClientSecretConfig string `yaml:"clientSecretConfig" json:"clientSecretConfig"`
	// UserInfo optionally names the connected account (e.g. its email).
	UserInfo *OAuth2UserInfo `yaml:"userInfo" json:"-"`
	// APIHosts receive the access token (http runtime). Default: baseUrl host.
	// Entries are hosts, optionally with a port ("api.example.com:8443"), or
	// wildcards for subdomains ("*.example.com", at least two labels after the
	// "*."). The scheme must be https and the port 443 unless an entry names one.
	APIHosts []string `yaml:"apiHosts" json:"apiHosts"`
	// InstanceURLField names the token response field that holds the API base
	// URL of the account (e.g. instance_url at Salesforce). It is stored with
	// the connection and available to manifest templates as
	// {{ oauth.instanceUrl }}. It must be an https origin allowed by APIHosts.
	InstanceURLField string `yaml:"instanceUrlField" json:"-"`
}

type OAuth2UserInfo struct {
	URL   string `yaml:"url"`
	Field string `yaml:"field"` // top-level field of the JSON response, e.g. email
}

// ConnectionParam is the name of the parameter that OAuth2 modules add to
// every node; it holds the id of the connected account to use.
const ConnectionParam = "connection"

type ManifestNode struct {
	Type        string `yaml:"type"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Icon overrides the module icon for this node (same formats).
	Icon    string   `yaml:"icon"`
	IconURI string   `yaml:"-"`
	Params  []Param  `yaml:"params"`
	Inputs  []string `yaml:"inputs"`  // default [main]
	Outputs []string `yaml:"outputs"` // default [main]
	// OutputSchema describes the emitted items, for the editor's field picker.
	OutputSchema *OutputSchema `yaml:"outputSchema"`
	// Trigger "poll" makes the node a polling trigger: it is checked every few
	// minutes while the workflow is active and starts a run with new items.
	Trigger string `yaml:"trigger"`
	// PollKey (http runtime) identifies items, e.g. {{ json.id }}: items whose
	// key was emitted before are not new. Go modules track new items themselves.
	PollKey string `yaml:"pollKey"`

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
var reservedModules = map[string]bool{BuiltinModule: true, "core": true, "trigger": true, "logic": true, "flow": true, "data": true}

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
	case "http", "wasm", "go":
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
	if m.OAuth2 != nil {
		if err := m.validateOAuth2(); err != nil {
			return fmt.Errorf("oauth2: %w", err)
		}
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
		if err := n.OutputSchema.Validate(append(append([]Param{}, m.Params...), n.Params...)); err != nil {
			return fmt.Errorf("node %s: %w", n.Type, err)
		}
		if m.Runtime == "http" && n.Request.URL == "" && m.BaseURL == "" {
			return fmt.Errorf("node %s: request.url or module baseUrl required", n.Type)
		}
		switch n.Trigger {
		case "":
			if n.PollKey != "" {
				return fmt.Errorf("node %s: pollKey requires trigger: poll", n.Type)
			}
		case string(TriggerPoll):
			if m.Runtime == "wasm" {
				return fmt.Errorf("node %s: wasm plugins cannot provide polling triggers yet", n.Type)
			}
			if m.Runtime == "http" && n.PollKey == "" {
				n.PollKey = "{{ json.id }}"
			}
			if len(n.Inputs) > 0 {
				return fmt.Errorf("node %s: triggers have no inputs", n.Type)
			}
			n.Inputs = []string{}
			for _, p := range n.Params {
				if p.Name == PollParam {
					return fmt.Errorf("node %s: parameter %q is reserved", n.Type, PollParam)
				}
			}
		default:
			return fmt.Errorf("node %s: unknown trigger %q (supported: poll)", n.Type, n.Trigger)
		}
		if len(n.Inputs) == 0 && n.Trigger == "" {
			n.Inputs = []string{"main"}
		}
		if len(n.Outputs) == 0 {
			n.Outputs = []string{"main"}
		}
	}
	return nil
}

func (m *Manifest) validateOAuth2() error {
	o := m.OAuth2
	for name, u := range map[string]string{"authorizeUrl": o.AuthorizeURL, "tokenUrl": o.TokenURL} {
		if err := checkProviderURL(u); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if o.UserInfo != nil {
		if err := checkProviderURL(o.UserInfo.URL); err != nil {
			return fmt.Errorf("userInfo.url: %w", err)
		}
	}
	if o.ScopeSeparator == "" {
		o.ScopeSeparator = " "
	}
	switch o.TokenAuth {
	case "":
		o.TokenAuth = "body"
	case "body", "basic":
	default:
		return fmt.Errorf("tokenAuth must be body or basic")
	}
	if o.ClientIDConfig == "" {
		o.ClientIDConfig = "clientId"
	}
	if o.ClientSecretConfig == "" {
		o.ClientSecretConfig = "clientSecret"
	}
	// The app credentials are plugin config: set per organization by admins.
	found := map[string]bool{}
	for _, p := range m.Config {
		found[p.Name] = true
		if p.Name == o.ClientSecretConfig && p.Type != ParamSecret {
			return fmt.Errorf("config %q must be of type secret", p.Name)
		}
	}
	for _, name := range []string{o.ClientIDConfig, o.ClientSecretConfig} {
		if !found[name] {
			return fmt.Errorf("config must declare %q (the OAuth app credentials)", name)
		}
	}
	if len(o.APIHosts) == 0 && m.Runtime == "http" {
		u, err := url.Parse(m.BaseURL)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("apiHosts is required when the module has no baseUrl")
		}
		o.APIHosts = []string{u.Host}
	}
	for _, h := range o.APIHosts {
		if err := checkAPIHost(h); err != nil {
			return fmt.Errorf("apiHosts: %w", err)
		}
	}
	if o.InstanceURLField != "" && !nameRe.MatchString(o.InstanceURLField) {
		return fmt.Errorf("instanceUrlField %q is not a field name", o.InstanceURLField)
	}
	for _, p := range append(append([]Param{}, m.Params...), m.Config...) {
		if p.Name == ConnectionParam {
			return fmt.Errorf("parameter name %q is reserved for the account picker", ConnectionParam)
		}
	}
	return nil
}

// splitAPIHost splits an apiHosts entry into host and port (default 443).
func splitAPIHost(h string) (host, port string) {
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		return strings.Trim(h, "[]"), "443"
	}
	return host, port
}

// checkAPIHost validates an apiHosts entry: a host with an optional port, or a
// "*.suffix" wildcard with at least two labels in the suffix (never "*.com").
func checkAPIHost(h string) error {
	host, _ := splitAPIHost(h)
	if _, port, err := net.SplitHostPort(h); err == nil {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q has an invalid port", h)
		}
	}
	if host == "" || strings.ContainsAny(host, "/@?# ") {
		return fmt.Errorf("%q is not a host", h)
	}
	if !strings.Contains(host, "*") {
		return nil
	}
	suffix, ok := strings.CutPrefix(host, "*.")
	if !ok || strings.Contains(suffix, "*") {
		return fmt.Errorf("%q: a wildcard must be a prefix like *.example.com", h)
	}
	labels := strings.Split(suffix, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%q: a wildcard needs at least two labels after \"*.\"", h)
	}
	for _, l := range labels {
		if l == "" {
			return fmt.Errorf("%q is not a valid wildcard host", h)
		}
	}
	return nil
}

// hostMatches reports whether host (no port) matches an apiHosts host: equal,
// or for "*.suffix" a subdomain of suffix (not suffix itself).
func hostMatches(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok && strings.HasPrefix(suffix, ".") {
		return len(host) > len(suffix) && strings.EqualFold(host[len(host)-len(suffix):], suffix)
	}
	return strings.EqualFold(pattern, host)
}

// AllowsURL reports whether the access token may be sent to u: https (plain
// http only in tests) and a host and port matching an apiHosts entry.
func (o *OAuth2Spec) AllowsURL(u *url.URL) bool {
	if o == nil {
		return false
	}
	port := portOf(u)
	for _, h := range o.APIHosts {
		host, hp := splitAPIHost(h)
		if hostMatches(host, u.Hostname()) && (allowInsecureAPIHosts || u.Scheme == "https" && hp == port) {
			return true
		}
	}
	return false
}

// checkProviderURL requires https, except for localhost (development, tests).
func checkProviderURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return nil
	}
	return fmt.Errorf("%q must use https", raw)
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
		Color: m.Color, Icon: m.IconURI, Runtime: m.Runtime, Source: source, Config: m.Config, Vars: m.Vars, AllowedHosts: m.AllowedHosts,
		OAuth2: m.OAuth2,
	}
}

// NodeDefinition builds the editor definition for one manifest node.
func (m *Manifest) NodeDefinition(n ManifestNode, source string) Definition {
	var params []Param
	if m.OAuth2 != nil {
		params = append(params, Param{
			Name: ConnectionParam, Label: "Account", Type: ParamConnection, Required: true, NoExpression: true,
			Description: "Connected " + m.Name + " account used by this node.",
		})
	}
	params = append(append(params, m.Params...), n.Params...)
	var trigger TriggerKind
	if n.Trigger != "" {
		trigger = TriggerKind(n.Trigger)
		params = append(params, pollIntervalParam())
	}
	category := m.Name
	return Definition{
		Type: m.Module + "." + n.Type, Name: n.Name, Category: category, Description: n.Description,
		Color: m.Color, Icon: n.IconURI, Trigger: trigger, Inputs: n.Inputs, Outputs: n.Outputs, Params: params,
		OutputSchema: n.OutputSchema, Module: m.Module, Source: source,
	}
}

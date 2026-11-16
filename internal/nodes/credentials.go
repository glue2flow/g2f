package nodes

import (
	"fmt"
	"net/url"
	"strings"
)

// CredentialsSpec declares the fields of a module's credentials: named,
// shareable sets of API keys and account settings that users store once on
// the Connections page and select in nodes (see docs/connections.md).
//
// Values reach only the module: HTTP manifests read them as
// {{ credential.<field> }} in request templates, WASM plugins as
// input.credential, Go modules as ExecContext.Credential. Node fields never
// see them.
type CredentialsSpec struct {
	// Fields are the values a credential holds. Types: string, url (readable,
	// shown on the Connections page), secret and secretUrl (encrypted and
	// write-only). url and secretUrl values must point to an apiHosts host,
	// checked when the credential is saved and again for every request.
	Fields []Param `yaml:"fields" json:"fields"`
	// APIHosts are the only hosts requests of an http module may go to, with
	// the rules of oauth2.apiHosts. Default: the host of a fixed baseUrl (http)
	// or allowedHosts (wasm). Hosts come from the manifest, never from a
	// credential: a credential can only choose among them.
	APIHosts []string `yaml:"apiHosts" json:"apiHosts,omitempty"`
	// Optional lets nodes run without a credential (e.g. public data).
	Optional bool `yaml:"optional" json:"optional,omitempty"`
}

// maxCredentialValue bounds one stored field value.
const maxCredentialValue = 16 << 10

// IsSecret reports whether values of the parameter are stored encrypted and
// never returned by the API.
func (p *Param) IsSecret() bool { return p.Type == ParamSecret || p.Type == ParamSecretURL }

func (m *Manifest) validateCredentials() error {
	c := m.Credentials
	if len(c.Fields) == 0 {
		return fmt.Errorf("at least one field is required")
	}
	names := map[string]bool{}
	hasURL := false
	for i := range c.Fields {
		p := &c.Fields[i]
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("invalid field name %q", p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate field %q", p.Name)
		}
		names[p.Name] = true
		if p.Type == "" {
			p.Type = ParamString
		}
		if p.Label == "" {
			p.Label = p.Name
		}
		switch p.Type {
		case ParamString, ParamSecret:
		case ParamURL, ParamSecretURL:
			hasURL = true
		default:
			return fmt.Errorf("field %q has type %q (supported: string, secret, url, secretUrl)", p.Name, p.Type)
		}
		if p.IsSecret() && p.Default != nil {
			return fmt.Errorf("field %q: secret fields cannot have a default", p.Name)
		}
		if p.Default != nil {
			if _, ok := p.Default.(string); !ok {
				return fmt.Errorf("field %q: the default must be a string", p.Name)
			}
		}
	}
	if len(c.APIHosts) == 0 {
		switch m.Runtime {
		case "http":
			u, err := url.Parse(m.BaseURL)
			if err != nil || u.Hostname() == "" || strings.Contains(m.BaseURL, "{{") {
				return fmt.Errorf("apiHosts is required when the module has no fixed baseUrl")
			}
			c.APIHosts = []string{u.Host}
		case "wasm":
			c.APIHosts = append([]string{}, m.AllowedHosts...)
		}
	}
	for _, h := range c.APIHosts {
		if err := checkAPIHost(h); err != nil {
			return fmt.Errorf("apiHosts: %w", err)
		}
	}
	if hasURL && len(c.APIHosts) == 0 {
		return fmt.Errorf("url fields need apiHosts")
	}
	// Defaults are checked like stored values (e.g. a default API URL).
	for _, p := range c.Fields {
		if s, _ := p.Default.(string); s != "" {
			if err := c.CheckValue(p.Name, s); err != nil {
				return fmt.Errorf("default of %w", err)
			}
		}
	}
	return m.checkConnectionName()
}

// Field returns the declared field name, or nil.
func (c *CredentialsSpec) Field(name string) *Param {
	for i := range c.Fields {
		if c.Fields[i].Name == name {
			return &c.Fields[i]
		}
	}
	return nil
}

// Defaults returns the defaults of the non-secret fields, the values a node
// without a credential sees (optional credentials).
func (c *CredentialsSpec) Defaults() map[string]any {
	out := map[string]any{}
	for _, p := range c.Fields {
		if s, ok := p.Default.(string); ok && !p.IsSecret() {
			out[p.Name] = s
		}
	}
	return out
}

// CheckValue validates one non-empty value of field name: url fields must be
// an https URL (no userinfo or fragment) on an apiHosts host, so a credential
// can never send module requests elsewhere.
func (c *CredentialsSpec) CheckValue(name, value string) error {
	p := c.Field(name)
	if p == nil {
		return fmt.Errorf("unknown field %q", name)
	}
	if len(value) > maxCredentialValue {
		return fmt.Errorf("field %q is longer than %d bytes", name, maxCredentialValue)
	}
	if p.Type != ParamURL && p.Type != ParamSecretURL {
		return nil
	}
	u, err := url.Parse(value)
	switch {
	case err != nil || u.Hostname() == "" || (u.Scheme != "https" && !(allowInsecureAPIHosts && u.Scheme == "http")):
		return fmt.Errorf("field %q must be an https URL", name)
	case u.User != nil || u.Fragment != "" || strings.Contains(value, "#"):
		return fmt.Errorf("field %q must not contain credentials or a fragment", name)
	case !c.AllowsURL(u):
		// The value may be a secret: name only the host.
		return fmt.Errorf("field %q: host %s is not allowed for this module (allowed: %s)", name, u.Host, strings.Join(c.APIHosts, ", "))
	}
	return nil
}

// AllowsURL reports whether requests of a credential module may go to u.
func (c *CredentialsSpec) AllowsURL(u *url.URL) bool {
	return c != nil && hostsAllow(c.APIHosts, u)
}

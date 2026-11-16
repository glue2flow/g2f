package nodes

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

func salesforceNode(t *testing.T, node string, hosts []string) (Node, Definition) {
	t.Helper()
	raw, err := os.ReadFile("../../modules/salesforce.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if hosts != nil {
		m.OAuth2.APIHosts = hosts
	}
	list, err := HTTPModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.Definition().Type == "salesforce."+node {
			return n, n.Definition()
		}
	}
	t.Fatalf("no node %s", node)
	return nil, Definition{}
}

func runWithOAuth(n Node, def Definition, params map[string]any, oauth *OAuthToken) error {
	_, err := n.Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{Name: def.Name, Params: params}, Def: &def, Items: []Item{{}}, OAuth: oauth,
		Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return err
}

// The bundled Salesforce module sends its requests to the connection's own
// instance URL ({{ oauth.instanceUrl }}), and only there.
func TestSalesforceUsesInstanceURL(t *testing.T) {
	var path, auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"records":[]}`))
	}))
	defer api.Close()

	n, def := salesforceNode(t, "query", []string{"127.0.0.1"})
	oauth := &OAuthToken{AccessToken: "tok", InstanceURL: api.URL}
	if err := runWithOAuth(n, def, map[string]any{"soql": "SELECT Id FROM Lead"}, oauth); err != nil {
		t.Fatal(err)
	}
	if path != "/services/data/v62.0/query" || auth != "Bearer tok" {
		t.Fatalf("request: %s %q", path, auth)
	}

	// A connection without an instance URL asks for a reconnect instead of sending nothing useful.
	err := runWithOAuth(n, def, map[string]any{"soql": "SELECT Id FROM Lead"}, &OAuthToken{AccessToken: "tok"})
	if err == nil || !strings.Contains(err.Error(), "no instance URL") {
		t.Fatalf("missing instance URL: %v", err)
	}
}

func TestSalesforceTokenHosts(t *testing.T) {
	allowInsecureAPIHosts = false
	t.Cleanup(func() { allowInsecureAPIHosts = true })
	n, def := salesforceNode(t, "query", nil)
	d := n.(*declarativeNode)

	// The token is refused before any request is made.
	for _, inst := range []string{"https://my.salesforce.com", "https://evil.com", "https://acme.my.salesforce.com.evil.com", "https://evilmy.salesforce.com", "http://acme.my.salesforce.com", "https://acme.my.salesforce.com:8443"} {
		err := runWithOAuth(n, def, map[string]any{"soql": "SELECT Id FROM Lead"}, &OAuthToken{AccessToken: "tok", InstanceURL: inst})
		if err == nil || !strings.Contains(err.Error(), "refusing to send") {
			t.Errorf("%s: %v", inst, err)
		}
	}
	for u, want := range map[string]bool{
		"https://acme.my.salesforce.com/x":             true,
		"https://acme--sb.sandbox.my.salesforce.com/x": true,
		"https://ACME.my.salesforce.com/x":             true,
		"https://my.salesforce.com/x":                  false,
		"https://na1.salesforce.com/x":                 false,
		"https://evil.com/acme.my.salesforce.com":      false,
		"https://acme.my.salesforce.com.evil.com/x":    false,
		"https://acme.my.salesforce.com@evil.com/x":    false,
		"https://evil.com@acme.my.salesforce.com/x":    true, // host is acme.my.salesforce.com
	} {
		if got := d.tokenHostAllowed(u); got != want {
			t.Errorf("%s: got %v, want %v", u, got, want)
		}
	}
}

func TestOAuthInstanceURLTemplate(t *testing.T) {
	n, _ := salesforceNode(t, "get", nil)
	url, err := n.(*declarativeNode).requestURL(expression.Env{
		"oauth":  map[string]any{"instanceUrl": "https://acme.my.salesforce.com"},
		"params": map[string]any{"sobject": "Account", "recordId": "001x"},
	})
	if err != nil || url != "https://acme.my.salesforce.com/services/data/v62.0/sobjects/Account/001x" {
		t.Fatalf("%q %v", url, err)
	}
}

// Node fields written by workflow authors must not see the oauth scope.
func TestNodeFieldsCannotReadOAuth(t *testing.T) {
	var got captured
	sf := shippedModule(t, "salesforce.yaml", `{"id":"001x","success":true}`, &got)
	runShipped(t, sf["salesforce.create"], map[string]any{"sobject": "Account", "fields": map[string]any{
		"Name": "{{ oauth.instanceUrl }}", "Phone": "{{ oauth.accessToken }}", "Fax": "{{ oauth }}",
	}}, Item{})
	for _, k := range []string{"Name", "Phone", "Fax"} {
		if v := got.body[k]; v != nil && v != "" {
			t.Errorf("%s: node field read the oauth scope: %v", k, v)
		}
	}
}

func TestAPIHostValidation(t *testing.T) {
	manifest := func(host string) string {
		return `
module: demo
baseUrl: https://api.example.com
config:
  - { name: clientId }
  - { name: clientSecret, type: secret }
oauth2:
  authorizeUrl: https://example.com/auth
  tokenUrl: https://example.com/token
  apiHosts: ["` + host + `"]
nodes:
  - { type: get, name: Get, request: { path: /x } }
`
	}
	for host, ok := range map[string]bool{
		"api.example.com":         true,
		"api.example.com:8443":    true,
		"[::1]":                   true,
		"*.example.com":           true,
		"*.my.example.com:8443":   true,
		"*.com":                   false,
		"*":                       false,
		"*.":                      false,
		"*.example.com.":          false,
		"**.example.com":          false,
		"a.*.example.com":         false,
		"*example.com":            false,
		"foo*.example.com":        false,
		"*.*.example.com":         false,
		"https://api.example.com": false,
		"api.example.com/x":       false,
		"":                        false,
	} {
		_, err := ParseManifest([]byte(manifest(host)))
		if (err == nil) != ok {
			t.Errorf("%q: err %v, want ok=%v", host, err, ok)
		}
	}
}

func TestHostMatches(t *testing.T) {
	for _, c := range []struct {
		pattern, host string
		want          bool
	}{
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "A.Example.COM", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", ".example.com", false},
		{"*.example.com", "evilexample.com", false},
		{"*.example.com", "a.example.com.evil.com", false},
		{"example.com", "example.com", true},
		{"example.com", "a.example.com", false},
	} {
		if got := hostMatches(c.pattern, c.host); got != c.want {
			t.Errorf("%s ~ %s: got %v", c.pattern, c.host, got)
		}
	}
}

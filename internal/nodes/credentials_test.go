package nodes

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/workflow"
)

func TestCredentialsManifestValidation(t *testing.T) {
	cases := map[string]string{
		"both oauth2 and credentials": `
config: [{ name: clientId }, { name: clientSecret, type: secret }]
oauth2: { authorizeUrl: "https://example.com/a", tokenUrl: "https://example.com/t" }
credentials: { fields: [{ name: token, type: secret }] }`,
		"no fields":              `credentials: { fields: [] }`,
		"unknown field type":     `credentials: { fields: [{ name: n, type: number }] }`,
		"node param type":        `credentials: { fields: [{ name: n, type: json }] }`,
		"duplicate field":        `credentials: { fields: [{ name: a }, { name: a }] }`,
		"secret default":         `credentials: { fields: [{ name: a, type: secret, default: x }] }`,
		"wildcard tld":           `credentials: { fields: [{ name: a }], apiHosts: ["*.com"] }`,
		"host with path":         `credentials: { fields: [{ name: a }], apiHosts: ["api.example.com/x"] }`,
		"url default elsewhere":  `credentials: { fields: [{ name: u, type: url, default: "https://other.example.org" }] }`,
		"reserved param":         "params: [{ name: connection }]\ncredentials: { fields: [{ name: a }] }",
		"templated baseUrl only": "credentials: { fields: [{ name: u, type: url }] }\nbaseUrl: \"{{ credential.u }}\"",
	}
	for name, extra := range cases {
		raw := "module: bad\nnodes: [{ type: a, name: A }]\n" + extra
		if !strings.Contains(extra, "baseUrl") {
			raw += "\nbaseUrl: https://api.example.com"
		}
		if _, err := ParseManifest([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}

	m, err := ParseManifest([]byte(`
module: ok
baseUrl: https://api.example.com/v1
credentials:
  fields:
    - { name: token, type: secret, required: true }
    - { name: hook, type: secretUrl }
nodes: [{ type: a, name: A }]
`))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Credentials
	if len(c.APIHosts) != 1 || c.APIHosts[0] != "api.example.com" || c.Fields[0].Label != "token" {
		t.Fatalf("defaults: %+v", c)
	}
	def := m.NodeDefinition(m.Nodes[0], "test")
	if p := def.Param(ConnectionParam); p == nil || p.Type != ParamConnection || !p.Required || p.Label != "Credential" {
		t.Fatalf("credential picker: %+v", p)
	}

	// Go modules need no apiHosts unless a field is a URL.
	if _, err := ParseManifest([]byte("module: native\nruntime: go\ncredentials: { fields: [{ name: p, type: secret }] }\nnodes: [{ type: a, name: A }]")); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest([]byte("module: native\nruntime: go\ncredentials: { fields: [{ name: u, type: url }] }\nnodes: [{ type: a, name: A }]")); err == nil {
		t.Fatal("url field without apiHosts accepted")
	}
}

func TestCredentialURLValues(t *testing.T) {
	allowInsecureAPIHosts = false
	t.Cleanup(func() { allowInsecureAPIHosts = true })
	c := &CredentialsSpec{
		Fields:   []Param{{Name: "hook", Type: ParamSecretURL}, {Name: "note", Type: ParamString}},
		APIHosts: []string{"hooks.slack.com", "*.example.com", "alt.example.org:8443"},
	}
	for v, ok := range map[string]bool{
		"https://hooks.slack.com/services/T/B/X": true,
		"https://a.example.com/x?y=1":            true,
		"https://alt.example.org:8443/x":         true,
		"https://alt.example.org/x":              false, // port must match
		"http://hooks.slack.com/services/T":      false,
		"https://hooks.slack.com.evil.io/x":      false,
		"https://evil.io/hooks.slack.com":        false,
		"https://user:pw@hooks.slack.com/x":      false,
		"https://hooks.slack.com/x#frag":         false,
		"hooks.slack.com/services/T":             false,
	} {
		if err := c.CheckValue("hook", v); (err == nil) != ok {
			t.Errorf("%s: err=%v", v, err)
		}
	}
	// The error names the host, never the (secret) URL.
	if err := c.CheckValue("hook", "https://evil.io/secret-path"); err == nil || strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("error: %v", err)
	}
	if err := c.CheckValue("note", "anything"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckValue("nope", "x"); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func runWithCredential(n Node, def Definition, params map[string]any, cred map[string]any) error {
	_, err := n.Execute(context.Background(), &ExecContext{
		Node: &workflow.Node{Name: def.Name, Params: params}, Def: &def, Items: []Item{{}}, Credential: cred,
		Env: expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return err
}

func TestHTTPModuleCredential(t *testing.T) {
	var gotAuth, gotQuery string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
			return
		}
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer other.Close()
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)

	m, err := ParseManifest([]byte(`
module: keyed
baseUrl: ` + api.URL + `
credentials:
  fields: [{ name: token, type: secret, required: true }]
headers:
  Authorization: "Bearer {{ credential.token }}"
nodes:
  - type: get
    name: Get
    params: [{ name: q }, { name: url }]
    request: { url: "{{ (params.url ?? '') != '' ? params.url : '` + api.URL + `/x' }}", query: { q: "{{ params.q }}" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := HTTPModuleNodes(m, "test")
	node, def := list[0], list[0].Definition()
	cred := map[string]any{"token": "tok-123"}

	// The token reaches the API host; node fields cannot read it.
	if err := runWithCredential(node, def, map[string]any{"q": "[{{ credential.token }}]"}, cred); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-123" || gotQuery != "[]" {
		t.Fatalf("auth=%q q=%q", gotAuth, gotQuery)
	}

	// Another host is refused before anything is sent.
	err = runWithCredential(node, def, map[string]any{"url": otherURL + "/steal"}, cred)
	if err == nil || !strings.Contains(err.Error(), "refusing") || otherHits.Load() != 0 {
		t.Fatalf("expected refusal, got %v (hits %d)", err, otherHits.Load())
	}

	// A redirect to another host is not followed with the credential.
	err = runWithCredential(node, def, map[string]any{"url": api.URL + "/redirect?to=" + otherURL + "/x"}, cred)
	if err == nil || !strings.Contains(err.Error(), "redirect") || otherHits.Load() != 0 {
		t.Fatalf("expected refused redirect, got %v (hits %d)", err, otherHits.Load())
	}
}

func bundledModule(t *testing.T, file string) *Manifest {
	t.Helper()
	raw, err := os.ReadFile("../../modules/" + file)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func moduleNode(t *testing.T, m *Manifest, typ string) (Node, Definition) {
	t.Helper()
	list, err := HTTPModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.Definition().Type == typ {
			return n, n.Definition()
		}
	}
	t.Fatalf("no node %s", typ)
	return nil, Definition{}
}

// The bundled GitHub module takes its token (and optionally an Enterprise
// Cloud API URL) from a credential and only talks to its apiHosts.
func TestGitHubCredential(t *testing.T) {
	var path, auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"full_name":"octo/hello"}`))
	}))
	defer api.Close()

	m := bundledModule(t, "github.yaml")
	c := m.Credentials
	if c == nil || !c.Optional || c.Field("token").Type != ParamSecret || c.Defaults()["apiUrl"] != "https://api.github.com" {
		t.Fatalf("credentials: %+v", c)
	}
	if err := c.CheckValue("apiUrl", "https://api.acme.ghe.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckValue("apiUrl", "https://github.example.com/api/v3"); err == nil {
		t.Fatal("self-hosted API URL accepted")
	}
	n, def := moduleNode(t, m, "github.repo.get")
	if p := def.Param(ConnectionParam); p == nil || p.Required {
		t.Fatalf("optional credential picker: %+v", p)
	}
	for _, p := range def.Params {
		if p.Type == ParamSecret {
			t.Fatalf("per-node secret %q left", p.Name)
		}
	}
	params := map[string]any{"owner": "octo", "repo": "hello"}

	// A credential URL outside apiHosts (e.g. stored before the manifest
	// changed) is refused at request time.
	err := runWithCredential(n, def, params, map[string]any{"token": "ghp_x", "apiUrl": api.URL})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}

	m.Credentials.APIHosts = []string{"127.0.0.1"}
	if err := runWithCredential(n, def, params, map[string]any{"token": "ghp_x", "apiUrl": api.URL}); err != nil {
		t.Fatal(err)
	}
	if path != "/repos/octo/hello" || auth != "Bearer ghp_x" {
		t.Fatalf("request: %s %q", path, auth)
	}
}

// The Slack incoming webhook URL is a secretUrl field: requests go to the
// stored URL, and only if it is on an allowed host.
func TestSlackWebhookCredential(t *testing.T) {
	var path, text string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		text = string(b)
		_, _ = w.Write([]byte("ok"))
	}))
	defer hook.Close()

	m := bundledModule(t, "slack.yaml")
	if f := m.Credentials.Field("webhookUrl"); f == nil || f.Type != ParamSecretURL {
		t.Fatalf("webhookUrl field: %+v", f)
	}
	if err := m.Credentials.CheckValue("webhookUrl", hook.URL+"/services/T/B/X"); err == nil {
		t.Fatal("webhook URL on another host accepted")
	}
	n, def := moduleNode(t, m, "slack.webhook.post")
	cred := map[string]any{"webhookUrl": hook.URL + "/services/T/B/X"}
	if err := runWithCredential(n, def, map[string]any{"text": "hi"}, cred); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}

	m.Credentials.APIHosts = []string{"127.0.0.1"}
	if err := runWithCredential(n, def, map[string]any{"text": "hi"}, cred); err != nil {
		t.Fatal(err)
	}
	if path != "/services/T/B/X" || text != `{"text":"hi"}` {
		t.Fatalf("request: %s %s", path, text)
	}
}

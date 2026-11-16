package nodes

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glue2flow/g2f/internal/expression"
	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/workflow"
)

func TestHTTPRequestBlocksPrivateAddresses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	setPolicy(t, netguard.Policy{})

	for _, u := range []string{
		srv.URL, "http://localhost" + port, "http://10.1.2.3/", "http://169.254.169.254/latest/meta-data/",
		"http://[::1]" + port, "http://[fd00::1]/", "http://[::ffff:127.0.0.1]" + port, "http://0.0.0.0" + port,
		"http://192.168.0.1/", "http://172.16.0.1/", "http://100.64.0.1/",
	} {
		_, err := doHTTP(context.Background(), httpRequest{URL: u})
		if err == nil || !strings.Contains(err.Error(), "blocked: private and local addresses are not allowed") {
			t.Errorf("%s: expected block, got %v", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("a blocked request reached the server")
	}
}

func TestHTTPRequestAllowList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	for name, entries := range map[string][]string{
		"ip": {"127.0.0.1"}, "cidr": {"127.0.0.0/8"}, "name": {"localhost"}, "wildcard": {"*.localhost", "localhost"},
	} {
		allow, err := netguard.ParseAllowList(entries)
		if err != nil {
			t.Fatal(err)
		}
		setPolicy(t, netguard.Policy{Allow: allow})
		u := srv.URL
		if name == "name" || name == "wildcard" {
			u = "http://localhost" + port
		}
		if res, err := doHTTP(context.Background(), httpRequest{URL: u}); err != nil || res["body"] != "ok" {
			t.Errorf("%s: %v %v", name, res, err)
		}
	}
}

// Every redirect hop is checked: an allowed host cannot bounce the request to
// an address that is not.
func TestHTTPRequestRedirectToBlockedAddress(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
	}))
	defer first.Close()
	allow, _ := netguard.ParseAllowList([]string{"localhost"})
	setPolicy(t, netguard.Policy{Allow: allow})

	_, err := doHTTP(context.Background(), httpRequest{URL: "http://localhost" + first.URL[strings.LastIndex(first.URL, ":"):]})
	if err == nil || !strings.Contains(err.Error(), "request to 127.0.0.1 blocked") {
		t.Fatalf("expected the redirect hop to be blocked, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect target was reached")
	}
}

// Credentials from a module's config (header, query) are not forwarded when a
// redirect leaves the API host.
func TestDeclarativeRedirectDoesNotLeakCredentials(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" || r.URL.Query().Get("key") != "" {
			leaked.Add(1)
		}
		leaked.Add(100)
	}))
	defer other.Close()
	var apiHits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits.Add(1)
		if r.URL.Path == "/redirect" {
			// Same host and port: followed, as before.
			http.Redirect(w, r, "/final?"+r.URL.RawQuery, http.StatusFound)
			return
		}
		if r.URL.Path == "/final" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, other.URL+"/steal?"+r.URL.RawQuery, http.StatusFound)
	}))
	defer api.Close()

	m, err := ParseManifest([]byte(`
module: demo
baseUrl: ` + api.URL + `
config:
  - { name: apiKey, type: secret }
nodes:
  - type: get
    name: Get
    params: [{ name: path }]
    request:
      path: "{{ params.path }}"
      headers: { X-Api-Key: "{{ config.apiKey }}" }
      query: { key: "{{ config.apiKey }}" }
`))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := HTTPModuleNodes(m, "test")
	def := list[0].Definition()
	run := func(path string) error {
		_, err := list[0].Execute(context.Background(), &ExecContext{
			Node: &workflow.Node{Name: "Get", Params: map[string]any{"path": path}}, Def: &def, Items: []Item{{}},
			Config: map[string]any{"apiKey": "s3cr3t"},
			Env:    expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		return err
	}

	err = run("leak")
	if err == nil || !strings.Contains(err.Error(), "refused: module credentials are only sent to the configured API host") {
		t.Fatalf("expected the redirect to be refused, got %v", err)
	}
	if leaked.Load() != 0 {
		t.Fatalf("credentials reached the redirect target (%d)", leaked.Load())
	}
	if err := run("redirect"); err != nil {
		t.Fatalf("same-host redirect: %v", err)
	}
}

func TestTokenHostRequiresHTTPSAndPort(t *testing.T) {
	allowInsecureAPIHosts = false
	t.Cleanup(func() { allowInsecureAPIHosts = true })
	mk := func(hosts ...string) *declarativeNode {
		return &declarativeNode{module: &Manifest{OAuth2: &OAuth2Spec{APIHosts: hosts}}}
	}
	d := mk("api.example.com", "alt.example.com:8443", "[::1]")
	for u, want := range map[string]bool{
		"https://api.example.com/x":      true,
		"https://API.example.com:443/x":  true,
		"http://api.example.com/x":       false,
		"https://api.example.com:8443/x": false,
		"https://alt.example.com:8443/x": true,
		"https://alt.example.com/x":      false,
		"https://evil.example.com/x":     false,
		"https://[::1]/x":                true,
	} {
		if got := d.tokenHostAllowed(u); got != want {
			t.Errorf("%s: got %v, want %v", u, got, want)
		}
	}
}

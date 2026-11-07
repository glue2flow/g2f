package nodes

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wolfposd/g8g/internal/expression"
	"github.com/wolfposd/g8g/internal/workflow"
)

func TestHTTPModuleOAuthToken(t *testing.T) {
	var gotAuth, gotQuery string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer api.Close()

	m, err := ParseManifest([]byte(`
module: demo
baseUrl: ` + api.URL + `
config:
  - { name: clientId }
  - { name: clientSecret, type: secret }
oauth2: { authorizeUrl: "https://example.com/a", tokenUrl: "https://example.com/t" }
nodes:
  - type: get
    name: Get
    params: [{ name: q }, { name: url }]
    request: { url: "{{ (params.url ?? '') != '' ? params.url : '` + api.URL + `/x' }}", query: { q: "{{ params.q }}" } }
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.OAuth2.APIHosts[0] != "127.0.0.1" || m.OAuth2.ClientIDConfig != "clientId" {
		t.Fatalf("defaults: %+v", m.OAuth2)
	}
	list, _ := HTTPModuleNodes(m, "test")
	node := list[0]
	def := node.Definition()
	if def.Params[0].Name != ConnectionParam || def.Params[0].Type != ParamConnection {
		t.Fatal("account picker parameter missing")
	}

	run := func(params map[string]any) error {
		_, err := node.Execute(context.Background(), &ExecContext{
			Node: &workflow.Node{Name: "Get", Params: params}, Def: &def, Items: []Item{{}},
			OAuth: &OAuthToken{AccessToken: "tok-123", TokenType: "bearer"},
			Env:   expression.Env{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		return err
	}

	// Injected as header; node fields cannot read it.
	if err := run(map[string]any{"q": "[{{ oauth.accessToken }}]"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-123" || gotQuery != "[]" {
		t.Fatalf("auth=%q q=%q", gotAuth, gotQuery)
	}

	// A URL from node parameters pointing elsewhere must not get the token.
	err = run(map[string]any{"url": strings.Replace(api.URL, "127.0.0.1", "localhost", 1) + "/steal"})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}

	// Manifests must declare the app credentials as config.
	if _, err := ParseManifest([]byte("module: bad\nbaseUrl: https://x\noauth2: {authorizeUrl: https://a, tokenUrl: https://t}\nnodes: [{type: a, name: A}]")); err == nil {
		t.Fatal("missing clientId/clientSecret config accepted")
	}
}

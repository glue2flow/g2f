package api

import (
	"context"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// Selecting a credential in a node needs access to it, like an OAuth2 account.
func TestCheckConnectionRefsCredential(t *testing.T) {
	m, err := nodes.ParseManifest([]byte(`
module: keyed
baseUrl: https://api.example.com
credentials: { fields: [{ name: token, type: secret }] }
nodes: [{ type: get, name: Get, request: { path: /x } }]
`))
	if err != nil {
		t.Fatal(err)
	}
	reg := nodes.NewRegistry()
	list, _ := nodes.HTTPModuleNodes(m, "test")
	if err := reg.RegisterModule(m.Info("test"), list); err != nil {
		t.Fatal(err)
	}
	conns := map[string]*store.Connection{
		"bobs":   {ID: "bobs", OrgID: "acme", Module: "keyed", Kind: store.KindCredential, OwnerSub: "bob", Scope: "private"},
		"shared": {ID: "shared", OrgID: "acme", Module: "keyed", Kind: store.KindCredential, OwnerSub: "bob", Scope: "group", Groups: []string{"/ops"}},
		"other":  {ID: "other", OrgID: "acme", Module: "github", Kind: store.KindCredential, OwnerSub: "alice", Scope: "private"},
	}
	get := func(_ context.Context, id string) (*store.Connection, error) {
		if c, ok := conns[id]; ok {
			return c, nil
		}
		return nil, store.ErrNotFound
	}
	alice := &auth.User{Sub: "alice", Org: "acme", Groups: []string{"/ops/oncall"}}
	graph := func(conn string) *workflow.Graph {
		return &workflow.Graph{Nodes: []workflow.Node{{ID: "n", Name: "Get", Type: "keyed.get", Params: map[string]any{nodes.ConnectionParam: conn}}}}
	}
	empty := &workflow.Graph{}
	for conn, ok := range map[string]bool{"bobs": false, "shared": true, "other": false, "missing": false, "": true} {
		err := checkConnectionRefs(context.Background(), get, reg, alice, empty, graph(conn))
		if (err == nil) != ok {
			t.Errorf("%q: %v", conn, err)
		}
		if err != nil && !strings.Contains(err.Error(), "not shared with you") {
			t.Errorf("%q: %v", conn, err)
		}
	}
	// A co-editor may keep someone else's selection unchanged.
	if err := checkConnectionRefs(context.Background(), get, reg, alice, graph("bobs"), graph("bobs")); err != nil {
		t.Fatal(err)
	}
}

func TestFieldsCannotReadCredential(t *testing.T) {
	def := nodes.Definition{Params: []nodes.Param{{Name: "text"}}}
	n := &workflow.Node{Name: "Get", Params: map[string]any{"text": "{{ credential.token }}"}}
	if err := checkFieldExpressions(n, def); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

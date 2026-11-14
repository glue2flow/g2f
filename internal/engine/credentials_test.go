package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

type fakeCredentials struct{ org, module, id string }

func (f *fakeCredentials) Credential(_ context.Context, org, module, id string) (map[string]any, []string, error) {
	f.org, f.module, f.id = org, module, id
	if id != "c1" {
		return nil, nil, errors.New("credential not found")
	}
	return map[string]any{"token": "s3cret-token", "region": "eu"}, []string{"s3cret-token"}, nil
}

// leakyNode logs and fails with its credential, and reports what its "text"
// field (written by the workflow author) evaluates to.
type leakyNode struct{ def nodes.Definition }

func (n *leakyNode) Definition() nodes.Definition { return n.def }

func (n *leakyNode) Execute(_ context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	text, err := ec.String("text", nodes.Item{}, 0)
	if err != nil {
		return nil, err
	}
	ec.Log(nodes.LogInfo, "using "+fmt.Sprint(ec.Credential["token"]))
	return nil, fmt.Errorf("API rejected %v in %v; field=%q", ec.Credential["token"], ec.Credential["region"], text)
}

func TestNodeCredential(t *testing.T) {
	m, err := nodes.ParseManifest([]byte(`
module: keyed
runtime: go
credentials: { fields: [{ name: token, type: secret, required: true }, { name: region }] }
nodes: [{ type: call, name: Call, params: [{ name: text }] }]
`))
	if err != nil {
		t.Fatal(err)
	}
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterModule(m.Info("test"), []nodes.Node{&leakyNode{m.NodeDefinition(m.Nodes[0], "test")}}); err != nil {
		t.Fatal(err)
	}
	src := &fakeCredentials{}
	e := New(reg, &memStore{execs: map[string]store.Execution{}}, discardLog(), Options{Credentials: src})
	run := func(params map[string]any) *store.Execution {
		wf := &workflow.Workflow{ID: "wf", Name: "creds", OrgID: "acme", Graph: workflow.Graph{
			Nodes: []workflow.Node{
				{ID: "t", Name: "Start", Type: "trigger.manual"},
				{ID: "k", Name: "Keyed", Type: "keyed.call", Params: params},
			},
			Edges: []workflow.Edge{{ID: "1", Source: "t", Target: "k"}},
		}}
		res, err := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
		if err != nil {
			t.Fatal(err)
		}
		return res.Execution
	}

	ex := run(map[string]any{nodes.ConnectionParam: "c1", "text": "[{{ credential.token }}]"})
	if src.org != "acme" || src.module != "keyed" || src.id != "c1" {
		t.Fatalf("resolved %+v", src)
	}
	// The node got the values; errors and logs are scrubbed; node fields saw nothing.
	if ex.Status != store.StatusError || strings.Contains(ex.Error, "s3cret") ||
		!strings.Contains(ex.Error, "API rejected •••••• in eu") || !strings.Contains(ex.Error, `field="[]"`) {
		t.Fatalf("error: %q", ex.Error)
	}
	step := ex.Steps[len(ex.Steps)-1]
	if strings.Contains(step.Error, "s3cret") || len(step.Logs) != 1 || strings.Contains(step.Logs[0].Message, "s3cret") {
		t.Fatalf("step: %+v", step)
	}

	if ex := run(map[string]any{"text": "x"}); !strings.Contains(ex.Error, "no credential selected") {
		t.Fatalf("missing credential: %q", ex.Error)
	}
	if ex := run(map[string]any{nodes.ConnectionParam: "other"}); !strings.Contains(ex.Error, "credential: credential not found") {
		t.Fatalf("unknown credential: %q", ex.Error)
	}
}

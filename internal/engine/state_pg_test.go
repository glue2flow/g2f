package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// TestPollAndStateWithPostgres runs a polling trigger, Remove Duplicates and
// the Data Store against a real database (G2F_TEST_DATABASE_URL).
func TestPollAndStateWithPostgres(t *testing.T) {
	url := os.Getenv("G2F_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("G2F_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	feed := []map[string]any{{"id": "a"}, {"id": "b"}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(feed)
	}))
	defer api.Close()

	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	m, err := nodes.ParseManifest([]byte("module: feed\nbaseUrl: " + api.URL + "\nnodes:\n  - { type: newEntry, name: New Entry, trigger: poll, request: { path: /x }, outputSchema: { dynamic: true } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := nodes.HTTPModuleNodes(m, "test")
	if err := reg.RegisterModule(m.Info("test"), list); err != nil {
		t.Fatal(err)
	}

	wf := &workflow.Workflow{OrgID: "o", Name: "poll-e2e", OwnerSub: "u", Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer, Active: true,
		Graph: workflow.Graph{
			Nodes: []workflow.Node{
				{ID: "p", Name: "New Entry", Type: "feed.newEntry", Params: map[string]any{}},
				{ID: "d", Name: "Dedupe", Type: "data.removeDuplicates", Params: map[string]any{"key": "{{ json.id }}"}},
				{ID: "c", Name: "Count", Type: "data.store", Params: map[string]any{"operation": "increment", "key": "seen"}},
			},
			Edges: []workflow.Edge{{ID: "e1", Source: "p", Target: "d"}, {ID: "e2", Source: "d", Target: "c"}},
		}}
	if err := st.CreateWorkflow(ctx, wf, nil); err != nil {
		t.Fatal(err)
	}
	defer st.DeleteWorkflow(ctx, wf.ID)
	eng := New(reg, st, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{State: st})

	// First check: remembers a and b, starts nothing.
	items, err := eng.Poll(ctx, wf, "p")
	if err != nil || len(items) != 0 {
		t.Fatalf("first check: %v %v", items, err)
	}
	mu.Lock()
	feed = append(feed, map[string]any{"id": "c"}, map[string]any{"id": "c"})
	mu.Unlock()
	if items, err = eng.Poll(ctx, wf, "p"); err != nil || len(items) != 1 || items[0]["id"] != "c" {
		t.Fatalf("second check: %v %v", items, err)
	}
	if items, _ = eng.Poll(ctx, wf, "p"); len(items) != 0 {
		t.Fatalf("third check repeated items: %v", items)
	}

	run := func(items []nodes.Item) {
		t.Helper()
		res, err := eng.Run(ctx, Request{Workflow: wf, TriggerType: nodes.TriggerPoll, TriggerNode: "p", Items: items, TriggeredBy: "poll"})
		if err != nil || res.Execution.Status != store.StatusSuccess {
			t.Fatalf("run: %v %+v", err, res.Execution)
		}
	}
	run([]nodes.Item{{"id": "c"}})
	run([]nodes.Item{{"id": "c"}, {"id": "d"}}) // c is a duplicate now
	v, ok, err := st.StateGet(ctx, wf.ID, "data", "seen")
	if err != nil || !ok || string(v) != "2" {
		t.Fatalf("counter = %s %v %v", v, ok, err)
	}

	// A failing check is recorded as a failed execution, once while the error repeats.
	api.Close()
	for range 2 {
		if _, err := eng.Poll(ctx, wf, "p"); err == nil {
			t.Fatal("check against a closed server succeeded")
		}
	}
	execs, err := st.ListExecutions(ctx, wf.ID, 10)
	if err != nil || len(execs) != 3 || execs[0].Status != store.StatusError || execs[0].TriggerType != "poll" {
		t.Fatalf("executions: %v %+v", err, execs[0])
	}
}

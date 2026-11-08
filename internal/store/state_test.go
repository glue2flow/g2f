package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/workflow"
)

// testStore connects to G2F_TEST_DATABASE_URL (a disposable database); the
// test is skipped without it.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("G2F_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("G2F_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWorkflowState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w := &workflow.Workflow{OrgID: "o", Name: "state", OwnerSub: "u", Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer}
	if err := s.CreateWorkflow(ctx, w, nil); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := s.StateGet(ctx, w.ID, "data", "a"); ok || err != nil {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	if err := s.StateSet(ctx, w.ID, "data", "a", json.RawMessage(`{"x":1}`), 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.StateGet(ctx, w.ID, "data", "a")
	if !ok || err != nil || string(v) != `{"x": 1}` {
		t.Fatalf("get: %s %v %v", v, ok, err)
	}
	// Expired values are invisible.
	if err := s.StateSet(ctx, w.ID, "data", "old", json.RawMessage(`1`), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok, _ := s.StateGet(ctx, w.ID, "data", "old"); ok {
		t.Fatal("expired value visible")
	}

	for want := 1.0; want <= 3; want++ {
		if n, err := s.StateIncrement(ctx, w.ID, "data", "count", 1, 0); err != nil || n != want {
			t.Fatalf("increment: %v %v", n, err)
		}
	}
	if n, err := s.StateIncrement(ctx, w.ID, "data", "old", 5, 0); err != nil || n != 5 {
		t.Fatalf("increment of expired value: %v %v", n, err)
	}
	list, err := s.StateList(ctx, w.ID, "data", "", 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v %v", list, err)
	}

	fresh, err := s.StateRemember(ctx, w.ID, "dedupe:x", []string{"k1", "k2"}, 0)
	if err != nil || !fresh[0] || !fresh[1] {
		t.Fatalf("remember: %v %v", fresh, err)
	}
	fresh, _ = s.StateRemember(ctx, w.ID, "dedupe:x", []string{"k2", "k3"}, time.Millisecond)
	if fresh[0] || !fresh[1] {
		t.Fatalf("remember again: %v", fresh)
	}
	time.Sleep(5 * time.Millisecond)
	seen, _ := s.StateSeen(ctx, w.ID, "dedupe:x", []string{"k1", "k3", "k4"})
	if !seen[0] || seen[1] || seen[2] {
		t.Fatalf("seen (k3 expired): %v", seen)
	}
	if fresh, _ = s.StateRemember(ctx, w.ID, "dedupe:x", []string{"k3"}, 0); !fresh[0] {
		t.Fatal("expired key not fresh again")
	}
	if _, err := s.PruneState(ctx); err != nil {
		t.Fatal(err)
	}
	// "old" was revived by the increment and k3 re-remembered: both survive pruning.
	if list, _ := s.StateList(ctx, w.ID, "data", "old", 10); len(list) != 1 {
		t.Fatal("revived value pruned")
	}
	if n, _ := s.StateClear(ctx, w.ID, "dedupe:x"); n != 3 {
		t.Fatalf("clear: %d", n)
	}
	if ok, _ := s.StateDelete(ctx, w.ID, "data", "a"); !ok {
		t.Fatal("delete")
	}
	// Deleting the workflow removes its state.
	if err := s.DeleteWorkflow(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.StateList(ctx, w.ID, "data", "", 10); len(list) != 0 {
		t.Fatalf("state survived workflow deletion: %v", list)
	}
}

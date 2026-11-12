package store

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/glue2flow/g2f/internal/workflow"
)

// testOwnDB is like testStore but creates and drops its own database, so
// tests that count rows are not disturbed by other data.
func testOwnDB(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv("G2F_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("G2F_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := "g2f_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPruneExecutions(t *testing.T) {
	s := testOwnDB(t)
	ctx := context.Background()
	w := &workflow.Workflow{OrgID: "o", Name: "prune", OwnerSub: "u", Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer}
	if err := s.CreateWorkflow(ctx, w, nil); err != nil {
		t.Fatal(err)
	}
	// add inserts an execution that started and finished age ago (finished
	// only for success/error).
	add := func(status ExecStatus, age string) string {
		t.Helper()
		e := &Execution{WorkflowID: w.ID, WorkflowVersion: 1, TriggerType: "manual", Status: status}
		if err := s.CreateExecution(ctx, e); err != nil {
			t.Fatal(err)
		}
		fin := "NULL"
		if status == StatusSuccess || status == StatusError {
			fin = "now() - interval '" + age + "'"
		}
		if _, err := s.pool.Exec(ctx, `UPDATE executions SET started_at = now() - interval '`+age+`', finished_at = `+fin+` WHERE id = $1`, e.ID); err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	exists := func(id string) bool {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	const retention = 30 * 24 * 60 * 60 * 1e9 // 30 days

	oldOK, oldErr := add(StatusSuccess, "40 days"), add(StatusError, "40 days")
	recentOK, recentErr := add(StatusSuccess, "1 day"), add(StatusError, "29 days")
	oldWaiting, oldRunning, oldQueued := add(StatusWaiting, "90 days"), add(StatusRunning, "90 days"), add(StatusQueued, "90 days")

	n, err := s.PruneExecutions(ctx, retention, 100)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v; want 2", n, err)
	}
	for name, id := range map[string]string{"old success": oldOK, "old error": oldErr} {
		if exists(id) {
			t.Errorf("%s was not deleted", name)
		}
	}
	for name, id := range map[string]string{"recent success": recentOK, "recent error": recentErr,
		"old waiting": oldWaiting, "old running": oldRunning, "old queued": oldQueued} {
		if !exists(id) {
			t.Errorf("%s was deleted", name)
		}
	}

	// The batch limit is respected, oldest first.
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, add(StatusSuccess, "50 days"))
	}
	if n, err := s.PruneExecutions(ctx, retention, 3); err != nil || n != 3 {
		t.Fatalf("limited batch pruned %d, %v; want 3", n, err)
	}
	if n, err := s.PruneExecutions(ctx, retention, 3); err != nil || n != 2 {
		t.Fatalf("second batch pruned %d, %v; want 2", n, err)
	}
	if n, err := s.PruneExecutions(ctx, retention, 3); err != nil || n != 0 {
		t.Fatalf("third batch pruned %d, %v; want 0", n, err)
	}
	for _, id := range ids {
		if exists(id) {
			t.Error("old execution left behind")
		}
	}
}

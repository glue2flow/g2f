package engine

// Recovery tests run against PostgreSQL, because claiming and fencing are SQL.
// Set G2F_TEST_DATABASE_URL to a server where the user may create databases,
// e.g. postgres://g2f:g2f@localhost:5432/g2f?sslmode=disable. Each test run
// creates and drops its own database.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

func testDB(t *testing.T) *store.Store {
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
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

// probe counts node runs; "block" nodes wait for gate (or cancellation) on
// their first run only.
type probe struct {
	mu      sync.Mutex
	runs    map[string]int
	gate    chan struct{}
	first   atomic.Bool
	entered chan struct{}
}

func newProbe() *probe {
	return &probe{runs: map[string]int{}, gate: make(chan struct{}), entered: make(chan struct{}, 4)}
}

func (p *probe) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[id]
}

type probeNode struct {
	p     *probe
	block bool
}

func (n probeNode) Definition() nodes.Definition {
	typ := "test.count"
	if n.block {
		typ = "test.block"
	}
	return nodes.Definition{Type: typ, Name: typ, Category: "Test", Module: nodes.BuiltinModule, Source: "builtin",
		Inputs: []string{"main"}, Outputs: []string{"main"}}
}

func (n probeNode) Execute(ctx context.Context, ec *nodes.ExecContext) (nodes.Output, error) {
	n.p.mu.Lock()
	n.p.runs[ec.Node.ID]++
	n.p.mu.Unlock()
	if n.block && n.p.first.CompareAndSwap(false, true) {
		n.p.entered <- struct{}{}
		select {
		case <-n.p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nodes.Output{"main": ec.Items}, nil
}

func recoveryEngine(t *testing.T, st *store.Store, p *probe) *Engine {
	t.Helper()
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	for _, n := range []nodes.Node{probeNode{p: p}, probeNode{p: p, block: true}} {
		if err := reg.Register(n); err != nil {
			t.Fatal(err)
		}
	}
	e := New(reg, st, discardLog(), Options{Workflows: st})
	if err := e.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

// count1 -> block -> count2
func recoveryWorkflow(t *testing.T, st *store.Store) *workflow.Workflow {
	t.Helper()
	wf := &workflow.Workflow{ID: uuid.NewString(), OrgID: "acme", Name: "Recover me", OwnerSub: "alice",
		Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer, Version: 1, Graph: workflow.Graph{
			Nodes: []workflow.Node{
				{ID: "t", Name: "Start", Type: "trigger.manual", Params: map[string]any{"payload": map[string]any{"n": 1}}},
				{ID: "c1", Name: "Count 1", Type: "test.count"},
				{ID: "b", Name: "Block", Type: "test.block"},
				{ID: "c2", Name: "Count 2", Type: "test.count"},
			},
			Edges: []workflow.Edge{{ID: "e1", Source: "t", Target: "c1"}, {ID: "e2", Source: "c1", Target: "b"}, {ID: "e3", Source: "b", Target: "c2"}},
		}}
	if err := st.CreateWorkflow(context.Background(), wf, nil); err != nil {
		t.Fatal(err)
	}
	return wf
}

func awaitStatus(t *testing.T, st *store.Store, id string, want store.ExecStatus) *store.Execution {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e, err := st.GetExecution(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if e.Status == want {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution %s is %s (%s), want %s", id, e.Status, e.Error, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func start(t *testing.T, e *Engine, wf *workflow.Workflow) string {
	t.Helper()
	exec, err := e.Start(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	return exec.ID
}

// An instance that stops sending heartbeats loses its executions to another
// instance, which continues after the last completed node. The stopped
// instance can no longer write the execution (fencing).
func TestRecoveryTakesOverStoppedInstance(t *testing.T) {
	st := testDB(t)
	p := newProbe()
	a := recoveryEngine(t, st, p)
	wf := recoveryWorkflow(t, st)
	id := start(t, a, wf)
	<-p.entered // A is inside "Block"; it sends no heartbeats from now on.

	b := recoveryEngine(t, st, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunRecovery(ctx, time.Second)

	done := awaitStatus(t, st, id, store.StatusSuccess)
	if done.Attempts != 1 {
		t.Fatalf("attempts = %d", done.Attempts)
	}
	if p.count("c1") != 1 || p.count("b") != 2 || p.count("c2") != 1 {
		t.Fatalf("runs %v: completed nodes must not run again", p.runs)
	}
	var warned bool
	for _, s := range done.Steps {
		for _, l := range s.Logs {
			warned = warned || (s.NodeID == "b" && strings.Contains(l.Message, "continued on another one"))
		}
	}
	if !warned {
		t.Fatalf("re-run node has no takeover note: %+v", done.Steps)
	}

	// A wakes up and goes on: its checkpoint is rejected, Count 2 does not run again.
	close(p.gate)
	time.Sleep(300 * time.Millisecond)
	if p.count("c2") != 1 {
		t.Fatalf("stopped instance continued the run: %v", p.runs)
	}
	if e, _ := st.GetExecution(context.Background(), id); e.Status != store.StatusSuccess {
		t.Fatalf("stopped instance overwrote the result: %s", e.Status)
	}
	a.Shutdown(0, time.Second)
}

// On graceful shutdown, executions that do not finish within the grace period
// are handed over immediately, without waiting for the instance timeout.
func TestRecoveryGracefulHandover(t *testing.T) {
	st := testDB(t)
	p := newProbe()
	a := recoveryEngine(t, st, p)
	wf := recoveryWorkflow(t, st)
	id := start(t, a, wf)
	<-p.entered

	a.Shutdown(100*time.Millisecond, 5*time.Second)
	released := awaitStatus(t, st, id, store.StatusQueued)
	if len(released.Steps) != 2 { // Start, Count 1
		t.Fatalf("released with steps %+v", released.Steps)
	}

	b := recoveryEngine(t, st, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := time.Now()
	go b.RunRecovery(ctx, time.Minute) // a long timeout: handover must not depend on it
	awaitStatus(t, st, id, store.StatusSuccess)
	if time.Since(began) > 5*time.Second {
		t.Fatal("handover took too long")
	}
	if p.count("c1") != 1 || p.count("b") != 2 || p.count("c2") != 1 {
		t.Fatalf("runs %v", p.runs)
	}
}

// Rows written by versions before checkpoints existed (no owner, no state)
// are only failed once they are older than the execution timeout, so a rolling
// upgrade does not touch runs of old instances.
func TestRecoveryLegacyExecutions(t *testing.T) {
	st := testDB(t)
	wf := recoveryWorkflow(t, st)
	ctx := context.Background()
	insert := func(age time.Duration) string {
		id := uuid.NewString()
		_, err := st.Pool().Exec(ctx, `INSERT INTO executions (id, workflow_id, workflow_version, trigger_type, status, started_at)
			VALUES ($1, $2, 1, 'manual', 'running', now() - make_interval(secs => $3))`, id, wf.ID, age.Seconds())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, recent := insert(time.Hour), insert(time.Second)

	e := recoveryEngine(t, st, newProbe())
	e.recoverOrphans(ctx, time.Second)
	got := awaitStatus(t, st, old, store.StatusError)
	if !strings.Contains(got.Error, "interrupted") {
		t.Fatalf("error %q", got.Error)
	}
	if r, _ := st.GetExecution(ctx, recent); r.Status != store.StatusRunning {
		t.Fatalf("recent legacy execution was touched: %s", r.Status)
	}
}

// An execution that keeps crashing its instances is given up.
func TestRecoveryGivesUp(t *testing.T) {
	st := testDB(t)
	p := newProbe()
	a := recoveryEngine(t, st, p)
	wf := recoveryWorkflow(t, st)
	id := start(t, a, wf)
	<-p.entered
	if _, err := st.Pool().Exec(context.Background(), `UPDATE executions SET attempts=$2 WHERE id=$1`, id, MaxRecoveries); err != nil {
		t.Fatal(err)
	}
	b := recoveryEngine(t, st, p)
	time.Sleep(1100 * time.Millisecond)
	b.recoverOrphans(context.Background(), time.Second)
	got := awaitStatus(t, st, id, store.StatusError)
	if !strings.Contains(got.Error, fmt.Sprintf("interrupted %d times", MaxRecoveries+1)) {
		t.Fatalf("error %q", got.Error)
	}
	close(p.gate)
	a.Shutdown(0, time.Second)
}

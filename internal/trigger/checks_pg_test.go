package trigger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/store/storetest"
	"github.com/glue2flow/g2f/internal/workflow"
)

// mailboxEngine is a polling source with a cursor that a check reads, then
// (after a pause) advances: two overlapping checks would emit mail twice.
type mailboxEngine struct {
	mu        sync.Mutex
	delivered int // messages 1..delivered exist
	cursor    int
	emitted   map[int]int
	running   atomic.Int32
	overlap   atomic.Bool
	checks    atomic.Int32
}

func (e *mailboxEngine) Poll(ctx context.Context, _ *workflow.Workflow, _ string) ([]nodes.Item, error) {
	if e.running.Add(1) > 1 {
		e.overlap.Store(true)
	}
	defer e.running.Add(-1)
	e.checks.Add(1)
	e.mu.Lock()
	cur, last := e.cursor, e.delivered
	e.mu.Unlock()
	time.Sleep(3 * time.Millisecond) // a check takes time
	var items []nodes.Item
	for id := cur + 1; id <= last; id++ {
		items = append(items, nodes.Item{"id": id})
	}
	e.mu.Lock()
	e.cursor = last
	e.mu.Unlock()
	return items, nil
}

func (e *mailboxEngine) Start(_ context.Context, r engine.Request) (*store.Execution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range r.Items {
		e.emitted[it["id"].(int)]++
	}
	return &store.Execution{ID: uuid.NewString()}, nil
}

func (e *mailboxEngine) Run(context.Context, engine.Request) (*engine.Result, error) {
	return nil, fmt.Errorf("not implemented")
}

// Two replicas on one database, woken alternately while mail arrives: their
// checks of the trigger never overlap and every message is emitted once.
func TestReplicasSerializeChecks(t *testing.T) {
	st := storetest.DB(t)
	ctx := context.Background()
	wf := &workflow.Workflow{ID: uuid.NewString(), OrgID: "acme", Name: "Mail", OwnerSub: "alice", Active: true,
		Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
			{ID: "p", Name: "New Thing", Type: "demo.newThing"},
		}}}
	if err := st.CreateWorkflow(ctx, wf, nil); err != nil {
		t.Fatal(err)
	}
	eng := &mailboxEngine{emitted: map[int]int{}}
	reg := pollReg(t)
	var replicas []*Checker
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for range 2 {
		c := newChecker(st, st.GetWorkflow, eng, reg, discard, uuid.NewString(), 4)
		c.sweep = 50 * time.Millisecond
		wg.Add(1)
		go func() { defer wg.Done(); c.Run(runCtx) }()
		replicas = append(replicas, c)
	}
	const total = 120
	var wakes sync.WaitGroup
	for i := 1; i <= total; i++ {
		eng.mu.Lock()
		eng.delivered = i
		eng.mu.Unlock()
		wakes.Add(1)
		go func(c *Checker) {
			defer wakes.Done()
			if err := c.Wake(ctx, wf.ID, "p"); err != nil {
				t.Error(err)
			}
		}(replicas[i%2])
		time.Sleep(time.Millisecond)
	}
	wakes.Wait()
	waitFor(t, "all mail", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return len(eng.emitted) == total
	})
	time.Sleep(200 * time.Millisecond)
	if eng.overlap.Load() {
		t.Fatal("two checks of the trigger ran at the same time")
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	for id, n := range eng.emitted {
		if n != 1 {
			t.Fatalf("message %d emitted %d times", id, n)
		}
	}
	if c := eng.checks.Load(); c >= total {
		t.Fatalf("%d checks for %d wake-ups: no coalescing", c, total)
	}
	t.Logf("%d wake-ups, %d checks", total, eng.checks.Load())
}

// The SQL behind the Checker: requests survive until a check that started
// after them completes; a claim expires when its replica stops renewing it.
func TestCheckClaimsSQL(t *testing.T) {
	st := storetest.DB(t)
	ctx := context.Background()
	wf := &workflow.Workflow{ID: uuid.NewString(), OrgID: "acme", Name: "Mail", OwnerSub: "alice",
		Scope: workflow.ScopePrivate, OrgRole: workflow.RoleViewer, Version: 1}
	if err := st.CreateWorkflow(ctx, wf, nil); err != nil {
		t.Fatal(err)
	}
	a, b := uuid.NewString(), uuid.NewString()
	must := func(ok bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if must(st.AcquireCheck(ctx, wf.ID, "p", a, time.Minute)) {
		t.Fatal("claimed without a request")
	}
	slot := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !must(st.RequestSlot(ctx, wf.ID, "p", slot)) || must(st.RequestSlot(ctx, wf.ID, "p", slot)) {
		t.Fatal("a slot must be requested exactly once")
	}
	if !must(st.AcquireCheck(ctx, wf.ID, "p", a, time.Minute)) || must(st.AcquireCheck(ctx, wf.ID, "p", b, time.Minute)) {
		t.Fatal("exactly one replica may run the check")
	}
	if err := st.RequestCheck(ctx, wf.ID, "p"); err != nil { // arrives while a's check runs
		t.Fatal(err)
	}
	if !must(st.ReleaseCheck(ctx, wf.ID, "p", a, true)) {
		t.Fatal("the request during the check must ask for a follow-up")
	}
	if !must(st.AcquireCheck(ctx, wf.ID, "p", b, time.Minute)) {
		t.Fatal("follow-up not claimable")
	}
	if must(st.ReleaseCheck(ctx, wf.ID, "p", b, true)) {
		t.Fatal("no follow-up expected")
	}
	// A check that could not start leaves the request open.
	_ = st.RequestCheck(ctx, wf.ID, "p")
	_ = must(st.AcquireCheck(ctx, wf.ID, "p", a, time.Minute))
	if !must(st.ReleaseCheck(ctx, wf.ID, "p", a, false)) {
		t.Fatal("request lost after a failed start")
	}
	// A stopped replica's claim expires and the request is offered again.
	_ = must(st.AcquireCheck(ctx, wf.ID, "p", a, time.Millisecond))
	time.Sleep(20 * time.Millisecond)
	refs, err := st.PendingChecks(ctx, 10)
	if err != nil || len(refs) != 1 || refs[0].WorkflowID != wf.ID {
		t.Fatalf("pending: %v %v", refs, err)
	}
	if !must(st.AcquireCheck(ctx, wf.ID, "p", b, time.Minute)) {
		t.Fatal("expired claim not taken over")
	}
	if must(st.ReleaseCheck(ctx, wf.ID, "p", a, true)) {
		t.Fatal("the old owner must not release the new claim")
	}
}

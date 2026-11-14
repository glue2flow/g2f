package trigger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// pollReg registers a polling trigger node from a manifest.
func pollReg(t *testing.T) *nodes.Registry {
	t.Helper()
	reg := registry(t)
	m, err := nodes.ParseManifest([]byte(`
module: demo
baseUrl: https://example.com
nodes:
  - { type: newThing, name: New Thing, trigger: poll, request: { path: /things }, outputSchema: { dynamic: true } }
`))
	if err != nil {
		t.Fatal(err)
	}
	list, err := nodes.HTTPModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterModule(m.Info("test"), list); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestSchedulerPolls(t *testing.T) {
	reg := pollReg(t)
	if reg.TriggerKind("demo.newThing") != nodes.TriggerPoll {
		t.Fatal("manifest node is not a polling trigger")
	}
	wf := &workflow.Workflow{ID: "wf", Active: true, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
		{ID: "p", Name: "New Thing", Type: "demo.newThing", Params: map[string]any{nodes.PollParam: 2.0}},
	}}}
	st := newFakeStore(wf)
	eng := &recordingEngine{ch: make(chan engine.Request, 10)}
	now := time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var replicas []*Scheduler
	for i := range 2 {
		s := NewScheduler(st, eng, reg, discard, SchedulerOptions{Instance: fmt.Sprintf("replica-%d", i)})
		s.now = func() time.Time { return now }
		s.sync(context.Background())
		go s.checks.Run(ctx)
		replicas = append(replicas, s)
	}
	// Slots are aligned to the interval plus the trigger's offset.
	off := pollOffset("wf", "p", 2*time.Minute)
	next := replicas[0].entries["wf"].jobs[0].next
	if want := NextPoll("wf", "p", 2*time.Minute, now); !next.Equal(want) || !next.Add(-off).Truncate(2*time.Minute).Equal(next.Add(-off)) {
		t.Fatalf("next check at %v, want %v (offset %v)", next, want, off)
	}
	tick := func() {
		now = now.Add(2 * time.Minute)
		for _, s := range replicas {
			s.fireDue(context.Background())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// No new items: checked once (one replica), no run.
	tick()
	eng.mu.Lock()
	if eng.polls != 1 || len(eng.reqs) != 0 {
		t.Fatalf("polls=%d runs=%d", eng.polls, len(eng.reqs))
	}
	eng.pollItems = []nodes.Item{{"id": 1}, {"id": 2}}
	eng.mu.Unlock()

	tick()
	select {
	case r := <-eng.ch:
		if r.TriggerType != nodes.TriggerPoll || r.TriggerNode != "p" || len(r.Items) != 2 {
			t.Fatalf("request %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no run for new items")
	}

	// A failing check starts no run (the engine records the failure).
	eng.mu.Lock()
	eng.pollItems, eng.pollErr = []nodes.Item{{"id": 3}}, errors.New("login failed")
	eng.mu.Unlock()
	tick()
	select {
	case r := <-eng.ch:
		t.Fatalf("run started despite failed check: %+v", r)
	default:
	}
}

// The offset spreads triggers over the interval; it is stable (every
// replica computes the same slots) and within the interval.
func TestPollOffset(t *testing.T) {
	d := 5 * time.Minute
	seen := map[time.Duration]bool{}
	for i := range 50 {
		wf, node := fmt.Sprintf("wf-%d", i), "n"
		off := pollOffset(wf, node, d)
		if off < 0 || off >= d || off%time.Second != 0 {
			t.Fatalf("offset %v outside [0, %v)", off, d)
		}
		if pollOffset(wf, node, d) != off {
			t.Fatal("offset not stable")
		}
		seen[off] = true
		now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 7 * time.Second)
		next := NextPoll(wf, node, d, now)
		if !next.After(now) || next.Sub(now) > d {
			t.Fatalf("next %v not within (now, now+%v]", next, d)
		}
		if again := NextPoll(wf, node, d, next); again.Sub(next) != d {
			t.Fatalf("slots not one interval apart: %v %v", next, again)
		}
	}
	if len(seen) < 40 {
		t.Fatalf("offsets not spread: %d distinct of 50", len(seen))
	}
}

func TestEffectiveInterval(t *testing.T) {
	if EffectiveInterval(5*time.Minute, false) != 5*time.Minute || EffectiveInterval(5*time.Minute, true) != PushSafetyNet ||
		EffectiveInterval(time.Hour, true) != time.Hour {
		t.Fatal("effective interval")
	}
}

type fakeHealth struct {
	mu   sync.Mutex
	refs []store.TriggerRef
}

func (h *fakeHealth) HealthyPushTriggers(context.Context) ([]store.TriggerRef, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]store.TriggerRef(nil), h.refs...), nil
}

// While push works the trigger is checked at the safety-net interval; when
// push stops working, the configured interval returns at the next sync.
func TestSafetyNetInterval(t *testing.T) {
	reg := pollReg(t)
	wf := &workflow.Workflow{ID: "wf", Active: true, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
		{ID: "p", Name: "New Thing", Type: "demo.newThing", Params: map[string]any{nodes.PollParam: 2.0}},
	}}}
	health := &fakeHealth{refs: []store.TriggerRef{{WorkflowID: "wf", NodeID: "p"}}}
	s := NewScheduler(newFakeStore(wf), &recordingEngine{}, reg, discard, SchedulerOptions{Health: health})
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.sync(context.Background())
	j := s.entries["wf"].jobs[0]
	if !j.push || j.interval() != PushSafetyNet || j.next.Sub(now) > PushSafetyNet {
		t.Fatalf("push healthy: interval %v next %v", j.interval(), j.next)
	}
	health.mu.Lock()
	health.refs = nil
	health.mu.Unlock()
	s.sync(context.Background())
	if j.push || j.interval() != 2*time.Minute || j.next.Sub(now) > 2*time.Minute {
		t.Fatalf("push lost: interval %v next %v", j.interval(), j.next)
	}
}

// checkerFixture runs one Checker over the fake store.
type blockingEngine struct {
	recordingEngine
	running, maxRunning atomic.Int32
	gate                chan struct{} // if set, each Poll waits for a value
	entered             chan struct{}
	delay               time.Duration // how long a check takes
}

func (e *blockingEngine) Poll(ctx context.Context, wf *workflow.Workflow, nodeID string) ([]nodes.Item, error) {
	n := e.running.Add(1)
	defer e.running.Add(-1)
	for {
		m := e.maxRunning.Load()
		if n <= m || e.maxRunning.CompareAndSwap(m, n) {
			break
		}
	}
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	time.Sleep(e.delay)
	return e.recordingEngine.Poll(ctx, wf, nodeID)
}

func (e *blockingEngine) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.polls
}

func startChecker(t *testing.T, st *fakeStore, eng Engine, workers int) *Checker {
	t.Helper()
	c := newChecker(st, st.GetWorkflow, eng, pollReg(t), discard, "me", workers)
	c.sweep = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func pollWF() *workflow.Workflow {
	return &workflow.Workflow{ID: "wf", Active: true, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
		{ID: "p", Name: "New Thing", Type: "demo.newThing"},
	}}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWakeRunsOneCheck(t *testing.T) {
	st := newFakeStore(pollWF())
	eng := &blockingEngine{recordingEngine: recordingEngine{ch: make(chan engine.Request, 10)}}
	c := startChecker(t, st, eng, 4)
	if err := c.Wake(context.Background(), "wf", "p"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "check", func() bool { return eng.count() == 1 })
	time.Sleep(100 * time.Millisecond) // sweeps must not repeat it
	if n := eng.count(); n != 1 {
		t.Fatalf("%d checks for one wake-up", n)
	}
}

// A burst that arrives within one check (a mail check takes a while) costs
// at most that check and one follow-up.
func TestWakeBurstCoalesces(t *testing.T) {
	st := newFakeStore(pollWF())
	eng := &blockingEngine{recordingEngine: recordingEngine{ch: make(chan engine.Request, 10)}, delay: 300 * time.Millisecond}
	c := startChecker(t, st, eng, 4)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Wake(context.Background(), "wf", "p") }()
	}
	wg.Wait()
	waitFor(t, "check", func() bool { return eng.count() >= 1 })
	time.Sleep(800 * time.Millisecond)
	if n := eng.count(); n < 1 || n > 2 {
		t.Fatalf("50 wake-ups made %d checks, want 1 or 2", n)
	}
	if eng.maxRunning.Load() != 1 {
		t.Fatalf("checks ran concurrently: %d", eng.maxRunning.Load())
	}
}

func TestWakeDuringCheckRunsOneFollowUp(t *testing.T) {
	st := newFakeStore(pollWF())
	eng := &blockingEngine{recordingEngine: recordingEngine{ch: make(chan engine.Request, 10)},
		gate: make(chan struct{}), entered: make(chan struct{}, 10)}
	c := startChecker(t, st, eng, 4)
	_ = c.Wake(context.Background(), "wf", "p")
	<-eng.entered
	for range 5 { // all while the first check runs
		_ = c.Wake(context.Background(), "wf", "p")
	}
	eng.gate <- struct{}{}
	<-eng.entered // the follow-up
	eng.gate <- struct{}{}
	waitFor(t, "follow-up", func() bool { return eng.count() == 2 })
	time.Sleep(150 * time.Millisecond)
	if n := eng.count(); n != 2 {
		t.Fatalf("%d checks, want the first plus exactly one follow-up", n)
	}
	if eng.maxRunning.Load() != 1 {
		t.Fatalf("checks ran concurrently")
	}
}

// A request whose check could not start (database error) is not lost: a
// later sweep runs it.
func TestFailedStartIsRetried(t *testing.T) {
	st := newFakeStore(pollWF())
	st.failGet = 2
	eng := &blockingEngine{recordingEngine: recordingEngine{ch: make(chan engine.Request, 10)}}
	c := startChecker(t, st, eng, 2)
	_ = c.Wake(context.Background(), "wf", "p")
	waitFor(t, "retried check", func() bool { return eng.count() == 1 })
}

// Checks run on a fixed number of workers.
func TestCheckWorkersBounded(t *testing.T) {
	var wfs []*workflow.Workflow
	for i := range 10 {
		w := pollWF()
		w.ID = fmt.Sprintf("wf-%d", i)
		wfs = append(wfs, w)
	}
	st := newFakeStore(wfs...)
	eng := &blockingEngine{recordingEngine: recordingEngine{ch: make(chan engine.Request, 10)},
		gate: make(chan struct{}), entered: make(chan struct{}, 20)}
	c := startChecker(t, st, eng, 3)
	for _, w := range wfs {
		_ = c.Wake(context.Background(), w.ID, "p")
	}
	for range 3 {
		<-eng.entered
	}
	time.Sleep(50 * time.Millisecond)
	if got := eng.running.Load(); got != 3 {
		t.Fatalf("%d checks running, want 3", got)
	}
	for range 10 {
		eng.gate <- struct{}{}
	}
	waitFor(t, "all checks", func() bool { return eng.count() == 10 })
}

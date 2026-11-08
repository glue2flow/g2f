package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
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
	var replicas []*Scheduler
	for range 2 {
		s := NewScheduler(st, eng, reg, discard, SchedulerOptions{})
		s.now = func() time.Time { return now }
		s.sync(context.Background())
		replicas = append(replicas, s)
	}
	// Slots are aligned to the interval: the next check is at 10:02.
	if next := replicas[0].entries["wf"].jobs[0].next; !next.Equal(time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("next check at %v", next)
	}
	tick := func() {
		now = now.Add(2 * time.Minute)
		for _, s := range replicas {
			s.fireDue(context.Background())
		}
		time.Sleep(50 * time.Millisecond)
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

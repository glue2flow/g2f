package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// waitStore is an in-memory ExecStore + WaitStore + WorkflowLoader.
type waitStore struct {
	mu     sync.Mutex
	n      int
	execs  map[string]store.Execution
	states map[string][]byte
	wfs    map[string]*workflow.Workflow
	done   chan string // receives ids of finished executions
}

func newWaitStore(wfs ...*workflow.Workflow) *waitStore {
	s := &waitStore{execs: map[string]store.Execution{}, states: map[string][]byte{},
		wfs: map[string]*workflow.Workflow{}, done: make(chan string, 20)}
	for _, w := range wfs {
		s.wfs[w.ID] = w
	}
	return s
}

func (s *waitStore) CreateExecution(_ context.Context, e *store.Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	e.ID = "exec-" + string(rune('0'+s.n))
	s.execs[e.ID] = *e
	return nil
}

func (s *waitStore) UpdateExecution(_ context.Context, e *store.Execution) error {
	s.mu.Lock()
	c := *e
	c.Steps = append([]store.Step(nil), e.Steps...)
	s.execs[e.ID] = c
	delete(s.states, e.ID)
	s.mu.Unlock()
	if e.Status == store.StatusSuccess || e.Status == store.StatusError {
		s.done <- e.ID
	}
	return nil
}

func (s *waitStore) PauseExecution(_ context.Context, e *store.Execution, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *e
	c.Steps = append([]store.Step(nil), e.Steps...)
	s.execs[e.ID] = c
	s.states[e.ID] = state
	return nil
}

func (s *waitStore) ClaimWaiting(_ context.Context, id, mode string) (*store.Execution, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.execs[id]
	if !ok || e.Status != store.StatusWaiting || (mode != "" && e.WaitMode != mode) {
		return nil, nil, store.ErrNotFound
	}
	e.Status = store.StatusRunning
	s.execs[id] = e
	return &e, s.states[id], nil
}

func (s *waitStore) DueWaiting(_ context.Context, now time.Time, _ int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, e := range s.execs {
		if e.Status == store.StatusWaiting && e.ResumeAt != nil && !e.ResumeAt.After(now) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *waitStore) GetWorkflow(_ context.Context, id string) (*workflow.Workflow, error) {
	if w, ok := s.wfs[id]; ok {
		return w, nil
	}
	return nil, store.ErrNotFound
}

func (s *waitStore) get(id string) store.Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execs[id]
}

func (s *waitStore) awaitDone(t *testing.T, id string) store.Execution {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case done := <-s.done:
			if done == id {
				return s.get(id)
			}
		case <-timeout:
			t.Fatalf("execution %s did not finish", id)
		}
	}
}

func waitEngine(t *testing.T, st *waitStore) *Engine {
	t.Helper()
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	return New(reg, st, discardLog(), Options{
		Workflows: st,
		ResumeURL: func(id string) string { return "https://g2f.test/webhook-resume/" + id + "/sig" },
	})
}

// approvalWF: Start -> Set(link) -> Wait -> main: Approved / timeout: Expired
func approvalWF(waitParams map[string]any) *workflow.Workflow {
	return &workflow.Workflow{ID: "wf", Name: "Approval", OrgID: "acme", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual", Params: map[string]any{"payload": map[string]any{"amount": 500}}},
			{ID: "ask", Name: "Ask", Type: "core.set", Params: map[string]any{
				"fields": map[string]any{"link": "{{ execution.resumeUrl }}"}}},
			{ID: "w", Name: "Wait", Type: "core.wait", Params: waitParams},
			{ID: "ok", Name: "Approved", Type: "core.set", Params: map[string]any{
				"fields": map[string]any{"decision": "{{ json.query.decision }}", "amount": "{{ trigger.amount }}"}, "keepExisting": false}},
			{ID: "exp", Name: "Expired", Type: "core.set", Params: map[string]any{"fields": map[string]any{"expired": true}}},
		},
		Edges: []workflow.Edge{
			{ID: "1", Source: "t", Target: "ask"},
			{ID: "2", Source: "ask", Target: "w"},
			{ID: "3", Source: "w", SourceHandle: "main", Target: "ok"},
			{ID: "4", Source: "w", SourceHandle: "timeout", Target: "exp"},
		},
	}}
}

func startAndPause(t *testing.T, e *Engine, st *waitStore, wf *workflow.Workflow) store.Execution {
	t.Helper()
	res, err := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	if res.Execution.Status != store.StatusWaiting {
		t.Fatalf("status %s (%s)", res.Execution.Status, res.Execution.Error)
	}
	return st.get(res.Execution.ID)
}

func stepOf(e store.Execution, id string) store.Step {
	for _, s := range e.Steps {
		if s.NodeID == id {
			return s
		}
	}
	return store.Step{}
}

func TestWaitWebhookResume(t *testing.T) {
	st := newWaitStore()
	e := waitEngine(t, st)
	exec := startAndPause(t, e, st, approvalWF(map[string]any{"mode": "webhook", "timeoutHours": 24.0}))

	if exec.WaitMode != nodes.WaitWebhook || exec.ResumeAt == nil {
		t.Fatalf("wait not recorded: %+v", exec)
	}
	link := stepOf(exec, "ask").Output["main"][0]["link"]
	if link != "https://g2f.test/webhook-resume/"+exec.ID+"/sig" {
		t.Fatalf("resume URL not available before the Wait node: %v", link)
	}
	if stepOf(exec, "w").Status != "waiting" {
		t.Fatal("wait step not marked waiting")
	}

	// The persisted state survives a JSON round trip: resume works from it alone.
	if _, err := e.Resume(context.Background(), exec.ID, nodes.Item{"query": map[string]any{"decision": "yes"}}); err != nil {
		t.Fatal(err)
	}
	done := st.awaitDone(t, exec.ID)
	if done.Status != store.StatusSuccess {
		t.Fatalf("status %s: %s", done.Status, done.Error)
	}
	got := stepOf(done, "ok").Output["main"][0]
	if got["decision"] != "yes" || got["amount"] != 500.0 {
		t.Fatalf("approved output %v", got)
	}
	if stepOf(done, "exp").Status != "skipped" || stepOf(done, "w").Status != "success" {
		t.Fatalf("branches: exp=%s w=%s", stepOf(done, "exp").Status, stepOf(done, "w").Status)
	}
	// Resuming twice is rejected.
	if _, err := e.Resume(context.Background(), exec.ID, nodes.Item{}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("second resume: %v", err)
	}
}

func TestWaitWebhookTimeout(t *testing.T) {
	st := newWaitStore()
	e := waitEngine(t, st)
	exec := startAndPause(t, e, st, approvalWF(map[string]any{"mode": "webhook"}))
	// Time-based resume of a webhook wait = timeout.
	if _, err := e.Resume(context.Background(), exec.ID, nil); err != nil {
		t.Fatal(err)
	}
	done := st.awaitDone(t, exec.ID)
	if stepOf(done, "exp").Status != "success" || stepOf(done, "ok").Status != "skipped" {
		t.Fatalf("expected timeout branch: ok=%s exp=%s", stepOf(done, "ok").Status, stepOf(done, "exp").Status)
	}
}

func TestWaitDurationViaResumer(t *testing.T) {
	st := newWaitStore()
	e := waitEngine(t, st)
	exec := startAndPause(t, e, st, approvalWF(map[string]any{"mode": "duration", "amount": 0.2, "unit": "seconds"}))
	// A webhook call must not resume a time-based wait.
	if _, err := e.Resume(context.Background(), exec.ID, nodes.Item{}); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("webhook resumed a duration wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunResumer(ctx, 50*time.Millisecond)
	done := st.awaitDone(t, exec.ID)
	if done.Status != store.StatusSuccess || stepOf(done, "ok").Status != "success" {
		t.Fatalf("status %s, ok=%s", done.Status, stepOf(done, "ok").Status)
	}
}

func TestSubWorkflow(t *testing.T) {
	child := &workflow.Workflow{ID: "child", Name: "Double", OrgID: "acme", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "c", Name: "Called", Type: "trigger.call"},
			{ID: "d", Name: "Double", Type: "core.code", Params: map[string]any{
				"code": "return [{ batch: items.map(i => i.n * 2) }];"}},
		},
		Edges: []workflow.Edge{{ID: "1", Source: "c", Target: "d"}},
	}}
	other := &workflow.Workflow{ID: "other", OrgID: "globex", Graph: child.Graph}
	parent := func(target string, params map[string]any) *workflow.Workflow {
		p := map[string]any{"workflow": target}
		for k, v := range params {
			p[k] = v
		}
		return &workflow.Workflow{ID: "parent", Name: "Parent", OrgID: "acme", Graph: workflow.Graph{
			Nodes: []workflow.Node{
				{ID: "t", Name: "Start", Type: "trigger.manual"},
				{ID: "items", Name: "Items", Type: "core.transform", Params: map[string]any{
					"expression": "{{ map(1..5, ({n: #})) }}"}},
				{ID: "x", Name: "Call", Type: "flow.executeWorkflow", Params: p},
			},
			Edges: []workflow.Edge{{ID: "1", Source: "t", Target: "items"}, {ID: "2", Source: "items", Target: "x"}},
		}}
	}
	st := newWaitStore(child, other)
	e := waitEngine(t, st)

	run := func(wf *workflow.Workflow) *Result {
		res, err := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := run(parent("child", map[string]any{"mode": "batch", "batchSize": 2.0}))
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("batch run: %s", res.Execution.Error)
	}
	if len(res.LastOutput) != 3 { // batches [1,2] [3,4] [5]
		t.Fatalf("batches: %v", res.LastOutput)
	}
	if b := res.LastOutput[2]["batch"].([]any); len(b) != 1 || b[0] != 10.0 {
		t.Fatalf("last batch %v", b)
	}

	res = run(parent("child", nil)) // once with all items
	if len(res.LastOutput) != 1 || len(res.LastOutput[0]["batch"].([]any)) != 5 {
		t.Fatalf("once: %v", res.LastOutput)
	}

	// Another organization's workflow is invisible.
	res = run(parent("other", nil))
	if res.Execution.Status != store.StatusError || !strings.Contains(res.Execution.Error, "not found") {
		t.Fatalf("cross-org call: %s", res.Execution.Error)
	}

	// Recursion is cut off.
	loop := &workflow.Workflow{ID: "loop", Name: "Loop", OrgID: "acme", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "c", Name: "Called", Type: "trigger.call"},
			{ID: "x", Name: "Again", Type: "flow.executeWorkflow", Params: map[string]any{"workflow": "loop"}},
		},
		Edges: []workflow.Edge{{ID: "1", Source: "c", Target: "x"}},
	}}
	st.wfs["loop"] = loop
	res = run(parent("loop", nil))
	if res.Execution.Status != store.StatusError || !strings.Contains(res.Execution.Error, "nested deeper") {
		t.Fatalf("recursion: %s", res.Execution.Error)
	}
}

func TestOnFailureHook(t *testing.T) {
	st := newWaitStore()
	reg := nodes.NewRegistry()
	_ = nodes.RegisterBuiltins(reg)
	called := make(chan string, 1)
	e := New(reg, st, discardLog(), Options{OnFailure: func(exec *store.Execution, wf *workflow.Workflow) {
		called <- wf.ID + ":" + exec.Error
	}})
	wf := &workflow.Workflow{ID: "wf", OrgID: "acme", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual"},
			{ID: "c", Name: "Boom", Type: "core.code", Params: map[string]any{"code": "throw new Error('boom')"}},
		},
		Edges: []workflow.Edge{{ID: "1", Source: "t", Target: "c"}},
	}}
	if _, err := e.Run(context.Background(), Request{Workflow: wf, TriggerType: nodes.TriggerManual}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-called:
		if !strings.HasPrefix(got, "wf:") || !strings.Contains(got, "boom") {
			t.Fatalf("hook got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnFailure not called")
	}
}

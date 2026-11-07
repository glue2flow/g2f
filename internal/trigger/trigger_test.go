package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

// fakeStore implements Store and engine.ExecStore in memory.
type fakeStore struct {
	mu      sync.Mutex
	wfs     map[string]*workflow.Workflow
	firings map[string]bool
	execs   map[string]store.Execution
	n       int
}

func newFakeStore(wfs ...*workflow.Workflow) *fakeStore {
	s := &fakeStore{wfs: map[string]*workflow.Workflow{}, firings: map[string]bool{}, execs: map[string]store.Execution{}}
	for _, w := range wfs {
		s.wfs[w.ID] = w
	}
	return s
}

func (s *fakeStore) ListActiveWorkflows(context.Context) ([]*workflow.Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*workflow.Workflow
	for _, w := range s.wfs {
		if w.Active {
			out = append(out, w)
		}
	}
	return out, nil
}

func (s *fakeStore) GetWorkflow(_ context.Context, id string) (*workflow.Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.wfs[id]; ok {
		return w, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeStore) ClaimFiring(_ context.Context, wf, node, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := wf + "/" + node + "/" + key
	if s.firings[k] {
		return false, nil
	}
	s.firings[k] = true
	return true, nil
}

func (s *fakeStore) HasFired(_ context.Context, wf, node, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firings[wf+"/"+node+"/"+key], nil
}

func (s *fakeStore) PruneFirings(context.Context, time.Duration) (int64, error) { return 0, nil }

func (s *fakeStore) CreateExecution(_ context.Context, e *store.Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	e.ID = fmt.Sprintf("exec-%d", s.n)
	s.execs[e.ID] = *e
	return nil
}

func (s *fakeStore) UpdateExecution(_ context.Context, e *store.Execution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs[e.ID] = *e
	return nil
}

// recordingEngine records Start calls.
type recordingEngine struct {
	mu   sync.Mutex
	reqs []engine.Request
	ch   chan engine.Request
}

func (e *recordingEngine) Start(_ context.Context, r engine.Request) (*store.Execution, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, r)
	e.mu.Unlock()
	e.ch <- r
	return &store.Execution{ID: "x"}, nil
}

func (e *recordingEngine) Run(context.Context, engine.Request) (*engine.Result, error) {
	return nil, fmt.Errorf("not implemented")
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func registry(t *testing.T) *nodes.Registry {
	t.Helper()
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	return reg
}

func scheduleWF(id string, active bool, params map[string]any) *workflow.Workflow {
	return &workflow.Workflow{ID: id, Active: active, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
		{ID: "sched", Name: "Every minute", Type: "trigger.schedule", Params: params},
	}}}
}

func TestSchedulerCronFiresOnceAcrossReplicas(t *testing.T) {
	reg := registry(t)
	st := newFakeStore(scheduleWF("wf1", true, map[string]any{"mode": "cron", "cron": "* * * * *", "timezone": "Europe/Berlin"}))
	eng := &recordingEngine{ch: make(chan engine.Request, 10)}

	now := time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC)
	clock := func() time.Time { return now }
	var replicas []*Scheduler
	for range 2 {
		s := NewScheduler(st, eng, reg, discard, SchedulerOptions{})
		s.now = clock
		s.sync(context.Background())
		replicas = append(replicas, s)
	}

	if d := replicas[0].untilNext(); d != 30*time.Second {
		t.Fatalf("next fire in %v, want 30s", d)
	}
	now = now.Add(30*time.Second + time.Millisecond)
	for _, s := range replicas {
		s.fireDue(context.Background())
	}

	select {
	case r := <-eng.ch:
		if r.TriggerType != nodes.TriggerSchedule || r.TriggerNode != "sched" {
			t.Fatalf("unexpected request %+v", r)
		}
		if got := r.Items[0]["scheduledAt"]; got != "2026-01-01T11:01:00+01:00" {
			t.Fatalf("scheduledAt = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("schedule did not fire")
	}
	select {
	case <-eng.ch:
		t.Fatal("fired twice across replicas")
	case <-time.After(100 * time.Millisecond):
	}
	if next := replicas[0].entries["wf1"].jobs[0].next; !next.Equal(time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("next fire at %v, want 10:02 UTC", next)
	}
}

func TestSchedulerOnce(t *testing.T) {
	reg := registry(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		at    string
		fires bool
	}{
		{"missed recently (catch up)", "2026-01-01T11:30", true},
		{"missed long ago", "2025-12-31T09:00", false},
		{"in the future", "2026-01-01T12:10", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newFakeStore(scheduleWF("wf", true, map[string]any{"mode": "once", "at": c.at}))
			eng := &recordingEngine{ch: make(chan engine.Request, 10)}
			s := NewScheduler(st, eng, reg, discard, SchedulerOptions{CatchUp: time.Hour})
			s.now = func() time.Time { return now }
			s.sync(context.Background())
			s.fireDue(context.Background())
			select {
			case <-eng.ch:
				if !c.fires {
					t.Fatal("unexpected firing")
				}
			case <-time.After(100 * time.Millisecond):
				if c.fires {
					t.Fatal("expected firing")
				}
			}
			// A resync must not fire it again.
			s.entries = map[string]*wfEntry{}
			s.sync(context.Background())
			s.fireDue(context.Background())
			select {
			case <-eng.ch:
				t.Fatal("one-time schedule fired twice")
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestSchedulerIgnoresInactive(t *testing.T) {
	st := newFakeStore(scheduleWF("wf", false, map[string]any{"cron": "* * * * *"}))
	s := NewScheduler(st, &recordingEngine{}, registry(t), discard, SchedulerOptions{})
	s.sync(context.Background())
	if len(s.entries) != 0 {
		t.Fatal("inactive workflow scheduled")
	}
	// Activating via the change listener schedules it immediately.
	w := scheduleWF("wf", true, map[string]any{"cron": "* * * * *"})
	s.WorkflowChanged(w)
	if len(s.entries["wf"].jobs) != 1 {
		t.Fatal("activated workflow not scheduled")
	}
	s.WorkflowDeleted("wf")
	if len(s.entries) != 0 {
		t.Fatal("deleted workflow still scheduled")
	}
}

func TestParseScheduleRejectsSubMinute(t *testing.T) {
	if _, err := nodes.ParseSchedule(map[string]any{"cron": "@every 10s"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := nodes.ParseSchedule(map[string]any{"cron": "@every 5m"}); err != nil {
		t.Fatal(err)
	}
}

func webhookWF(active bool, params map[string]any) *workflow.Workflow {
	return &workflow.Workflow{ID: "wf", Active: active, Version: 1, Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "hook", Name: "Hook", Type: "trigger.webhook", Params: params},
			{ID: "set", Name: "Reply", Type: "core.set", Params: map[string]any{
				"fields": map[string]any{"hello": "{{ json.body.name }}", "q": "{{ json.query.x }}"}, "keepExisting": false}},
		},
		Edges: []workflow.Edge{{ID: "e", Source: "hook", Target: "set"}},
	}}
}

func serveWebhook(t *testing.T, wf *workflow.Workflow, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	reg := registry(t)
	st := newFakeStore(wf)
	eng := engine.New(reg, st, discard, engine.Options{})
	h := NewWebhookHandler(st, eng, reg, nil, discard, 5*time.Second)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhook(t *testing.T) {
	body := `{"name":"Ada"}`
	mac := hmac.New(sha256.New, []byte("k3y"))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	cases := []struct {
		name   string
		wf     *workflow.Workflow
		method string
		hdr    map[string]string
		status int
	}{
		{"inactive", webhookWF(false, nil), "POST", nil, 404},
		{"wrong method", webhookWF(true, nil), "GET", nil, 405},
		{"no auth, async", webhookWF(true, nil), "POST", nil, 202},
		{"token missing", webhookWF(true, map[string]any{"auth": "token", "secret": "s3cret"}), "POST", nil, 401},
		{"token ok", webhookWF(true, map[string]any{"auth": "token", "secret": "s3cret"}), "POST", map[string]string{"X-G2F-Token": "s3cret"}, 202},
		{"hmac bad", webhookWF(true, map[string]any{"auth": "hmac", "secret": "k3y"}), "POST", map[string]string{"X-Signature-256": "sha256=00"}, 401},
		{"hmac ok", webhookWF(true, map[string]any{"auth": "hmac", "secret": "k3y"}), "POST", map[string]string{"X-Signature-256": sig}, 202},
		{"any method", webhookWF(true, map[string]any{"method": "ANY"}), "PUT", nil, 202},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := serveWebhook(t, c.wf, c.method, "/webhook/wf/hook?x=1", body, c.hdr)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
		})
	}
}

func TestWebhookRespondLastNode(t *testing.T) {
	wf := webhookWF(true, map[string]any{"respond": "lastNode", "auth": "token", "secret": "s"})
	rec := serveWebhook(t, wf, "POST", "/webhook/wf/hook?x=42", `{"name":"Ada"}`, map[string]string{"X-G2F-Token": "s"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["hello"] != "Ada" || got["q"] != "42" {
		t.Fatalf("unexpected response %v", got)
	}
	if rec.Header().Get("X-G2F-Execution-Id") == "" {
		t.Fatal("missing execution id header")
	}
}

func TestWebhookStripsSecretHeader(t *testing.T) {
	cfg := &nodes.Webhook{Auth: "token", Header: "X-G2F-Token"}
	r := httptest.NewRequest(http.MethodPost, "/webhook/a/b", nil)
	r.Header.Set("X-G2F-Token", "secret")
	r.Header.Set("Cookie", "session=1")
	r.Header.Set("X-Other", "ok")
	item := requestItem(r, nil, cfg)
	h := item["headers"].(map[string]any)
	if _, ok := h["x-g2f-token"]; ok {
		t.Fatal("secret header leaked into item")
	}
	if _, ok := h["cookie"]; ok {
		t.Fatal("cookie leaked into item")
	}
	if h["x-other"] != "ok" {
		t.Fatal("regular header missing")
	}
}

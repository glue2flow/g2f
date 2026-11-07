package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

type errStore struct{ wfs []*workflow.Workflow }

func (s errStore) WorkflowsWithNodeType(_ context.Context, org, _ string) ([]*workflow.Workflow, error) {
	var out []*workflow.Workflow
	for _, w := range s.wfs {
		if w.OrgID == org {
			out = append(out, w)
		}
	}
	return out, nil
}

func errorWF(id string, active bool, params map[string]any) *workflow.Workflow {
	return &workflow.Workflow{ID: id, OrgID: "acme", Active: active, Graph: workflow.Graph{Nodes: []workflow.Node{
		{ID: "e", Name: "On error", Type: "trigger.error", Params: params},
	}}}
}

func TestErrorDispatcher(t *testing.T) {
	failed := &workflow.Workflow{ID: "orders", Name: "Orders", OrgID: "acme"}
	selfHandler := errorWF("orders", false, map[string]any{"scope": "self"})
	watcher := errorWF("alerts", true, map[string]any{"scope": "selected", "workflows": "orders, other"})
	inactive := errorWF("sleepy", false, map[string]any{"scope": "selected", "workflows": "orders"})
	unrelated := errorWF("misc", true, map[string]any{"scope": "selected", "workflows": "other"})

	cases := []struct {
		trigger string
		want    []string
	}{
		{"schedule", []string{"orders", "alerts"}},
		{"manual", nil}, // includeManual is off
		{"error", nil},  // failures of error handlers never cascade
	}
	for _, c := range cases {
		eng := &recordingEngine{ch: make(chan engine.Request, 10)}
		d := NewErrorDispatcher(errStore{[]*workflow.Workflow{selfHandler, watcher, inactive, unrelated}}, eng,
			registry(t), discard, "https://g2f.test")
		d.WorkflowFailed(&store.Execution{ID: "x1", TriggerType: c.trigger, Error: "node \"HTTP\": boom",
			Steps: []store.Step{{NodeName: "HTTP", Status: "error"}}, StartedAt: time.Now()}, failed)
		close(eng.ch)
		var got []string
		for r := range eng.ch {
			got = append(got, r.Workflow.ID)
			if r.TriggerType != nodes.TriggerError || r.Items[0]["failedNode"] != "HTTP" {
				t.Fatalf("bad request %+v", r)
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("trigger %s: started %v, want %v", c.trigger, got, c.want)
		}
	}
}

type fakeResumer struct{ got nodes.Item }

func (f *fakeResumer) Resume(_ context.Context, id string, item nodes.Item) (*store.Execution, error) {
	if id != "e1" {
		return nil, engine.ErrNotWaiting
	}
	f.got = item
	return &store.Execution{ID: id}, nil
}

func TestResumeHandler(t *testing.T) {
	signer := NewResumeSigner("k", "https://g2f.test/")
	res := &fakeResumer{}
	h := NewResumeHandler(signer, res, discard)
	url := signer.URL("e1")
	if !strings.HasPrefix(url, "https://g2f.test/webhook-resume/e1/") {
		t.Fatalf("url %s", url)
	}
	path := strings.TrimPrefix(url, "https://g2f.test")

	do := func(p string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec.Code
	}
	if code := do(path + "?decision=approve"); code != 202 {
		t.Fatalf("valid resume: %d", code)
	}
	if q := res.got["query"].(map[string]any); q["decision"] != "approve" {
		t.Fatalf("query not passed: %v", res.got)
	}
	if code := do("/webhook-resume/e1/deadbeef"); code != 404 {
		t.Fatalf("bad signature: %d", code)
	}
	if code := do(strings.TrimPrefix(signer.URL("e2"), "https://g2f.test")); code != 410 {
		t.Fatalf("not waiting: %d", code)
	}
	if NewResumeSigner("other-key", "x").sign("e1") == signer.sign("e1") {
		t.Fatal("signature must depend on the key")
	}
}

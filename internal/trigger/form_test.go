package trigger

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/workflow"
)

func formWF(active bool) *workflow.Workflow {
	return &workflow.Workflow{ID: "wf", Active: active, Version: 1, Graph: workflow.Graph{Nodes: []workflow.Node{
		{ID: "form", Name: "Form", Type: "trigger.form", Params: map[string]any{
			"title":       "Team <survey>",
			"description": "Tell us.\n\nThanks!",
			"fields": []any{
				map[string]any{"key": "name", "label": "Name", "type": "text", "required": true},
				map[string]any{"key": "team", "label": "Team", "type": "choice", "required": true, "options": []any{"Sales", "Support"}},
				map[string]any{"key": "topics", "label": "Topics", "type": "multichoice", "options": []any{"A", "B"}},
			},
		}},
		{ID: "hook", Name: "Hook", Type: "trigger.webhook"},
	}}}
}

func serveForm(t *testing.T, wf *workflow.Workflow, method, path string, form url.Values) (*httptest.ResponseRecorder, *recordingEngine) {
	t.Helper()
	reg := registry(t)
	eng := &recordingEngine{ch: make(chan engine.Request, 4)}
	h := NewFormHandler(newFakeStore(wf), eng, reg, discard, FormOptions{SecretKey: "k", State: newFakeState()})
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, eng
}

func TestFormGet(t *testing.T) {
	rec, _ := serveForm(t, formWF(true), "GET", "/form/wf/form?name=Ada", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	html := rec.Body.String()
	for _, want := range []string{
		"Team &lt;survey&gt;",                     // escaped title
		`name="team" value="Support"`,             // radio option
		`type="checkbox" name="topics" value="B"`, // checkbox option
		`value="Ada"`,                             // prefilled from the query
		`<p class="muted">Thanks!</p>`,            // paragraphs
	} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatal("not served as HTML")
	}
}

func TestFormUnavailable(t *testing.T) {
	cases := []struct {
		name, path string
		wf         *workflow.Workflow
	}{
		{"inactive", "/form/wf/form", formWF(false)},
		{"unknown workflow", "/form/nope/form", formWF(true)},
		{"not a form", "/form/wf/hook", formWF(true)},
		{"bad path", "/form/wf", formWF(true)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, _ := serveForm(t, c.wf, "GET", c.path, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d", rec.Code)
			}
		})
	}
}

func TestFormSubmit(t *testing.T) {
	rec, eng := serveForm(t, formWF(true), "POST", "/form/wf/form",
		url.Values{"name": {"Ada"}, "team": {"Sales"}, "topics": {"B", "A"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/form/wf/form?submitted" {
		t.Fatalf("status %d, location %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if len(eng.reqs) != 1 {
		t.Fatalf("%d runs started", len(eng.reqs))
	}
	r := eng.reqs[0]
	if r.TriggerType != nodes.TriggerForm || r.TriggerNode != "form" {
		t.Fatalf("unexpected request %+v", r)
	}
	item := r.Items[0]
	if item["name"] != "Ada" || item["team"] != "Sales" || len(item["topics"].([]any)) != 2 {
		t.Fatalf("unexpected item %v", item)
	}

	rec, _ = serveForm(t, formWF(true), "GET", "/form/wf/form?submitted", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Thank you") {
		t.Fatalf("success page: %d %s", rec.Code, rec.Body)
	}
}

func TestFormSubmitInvalid(t *testing.T) {
	rec, eng := serveForm(t, formWF(true), "POST", "/form/wf/form", url.Values{"name": {"Ada"}, "team": {"Other"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	if len(eng.reqs) != 0 {
		t.Fatal("invalid submission started a run")
	}
	html := rec.Body.String()
	if !strings.Contains(html, "Please choose one of the options.") || !strings.Contains(html, `value="Ada"`) {
		t.Fatalf("errors or kept answers missing: %s", html)
	}
}

func TestFormRunsEngine(t *testing.T) {
	// End to end with the real engine: the trigger kind must be accepted.
	wf := formWF(true)
	reg := registry(t)
	st := newFakeStore(wf)
	eng := engine.New(reg, st, discard, engine.Options{})
	h := NewFormHandler(st, eng, reg, discard, FormOptions{SecretKey: "k"})
	req := httptest.NewRequest("POST", "/form/wf/form", strings.NewReader("name=Ada&team=Support"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("X-G2F-Execution-Id") == "" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

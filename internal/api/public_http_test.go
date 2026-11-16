package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// These endpoints have unit tests in internal/trigger and internal/push; here
// they are only checked through the real router: reachable without a token,
// and behaving as documented.

func TestWebhookEndpoint(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	active := a.createWF("hook", hookGraph("tok-123", "token"), map[string]any{"active": true})["id"].(string)
	inactive := a.createWF("off", hookGraph("tok-123", "token"), nil)["id"].(string)
	post := func(path, token string) (int, string, http.Header) {
		hdr := http.Header{"Content-Type": {"application/json"}}
		if token != "" {
			hdr.Set("X-G2F-Token", token)
		}
		rec := s.anon().raw("POST", path, strings.NewReader(`{"n":1}`), hdr)
		return rec.Code, rec.Body.String(), rec.Header()
	}

	for _, c := range []struct {
		name, path string
		want       int
	}{
		{"unknown workflow", "/webhook/00000000-0000-0000-0000-000000000000/hook", http.StatusNotFound},
		{"malformed id", "/webhook/nope/hook", http.StatusNotFound},
		{"unknown node", "/webhook/" + active + "/log", http.StatusNotFound},
		{"path without node", "/webhook/" + active, http.StatusNotFound},
		{"inactive workflow", "/webhook/" + inactive + "/hook", http.StatusNotFound},
		{"missing token", "/webhook/" + active + "/hook", http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			tok := "tok-123"
			if c.name == "missing token" {
				tok = ""
			}
			if code, body, _ := post(c.path, tok); code != c.want {
				t.Fatalf("%d, want %d: %s", code, c.want, body)
			}
		})
	}
	if rec := s.anon().raw("GET", "/webhook/"+active+"/hook", nil, nil); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "POST" {
		t.Fatalf("wrong method: %d %v", rec.Code, rec.Header())
	}

	code, body, hdr := post("/webhook/"+active+"/hook", "tok-123")
	if code != http.StatusAccepted || hdr.Get("X-G2F-Execution-Id") == "" || !strings.Contains(body, hdr.Get("X-G2F-Execution-Id")) {
		t.Fatalf("active webhook: %d %s %v", code, body, hdr)
	}
	ex := a.waitExec(hdr.Get("X-G2F-Execution-Id"), finished)
	if ex["status"] != "success" || ex["triggerType"] != "webhook" || ex["triggeredBy"] != "webhook" {
		t.Fatalf("execution %v", ex)
	}
	if out := stepOutput(t, ex, "hook"); mustJSON(t, obj(t, out[0])["body"]) != `{"n":1}` {
		t.Fatalf("hook output %v", out)
	}

	t.Run("respond with the last node's output", func(t *testing.T) {
		g := hookGraph("<omit>", "none")
		g["nodes"].([]map[string]any)[0]["params"].(map[string]any)["respond"] = "lastNode"
		id := a.createWF("sync", g, map[string]any{"active": true})["id"].(string)
		code, body, _ := post("/webhook/"+id+"/hook", "")
		if code != http.StatusOK || !strings.Contains(body, `"body":{"n":1}`) {
			t.Fatalf("%d %s", code, body)
		}
	})
}

func formGraph(protection string) map[string]any {
	return graph([]map[string]any{
		node("form", "trigger.form", map[string]any{
			"title": "Team survey", "protection": protection,
			"fields": []any{map[string]any{"key": "name", "label": "Name", "type": "text", "required": true}},
		}),
		node("log", "core.log", map[string]any{"message": "{{ json.name }}"}),
	}, edge("form", "log"))
}

func TestFormEndpoint(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	id := a.createWF("form", formGraph("none"), map[string]any{"active": true})["id"].(string)
	inactive := a.createWF("closed", formGraph("none"), nil)["id"].(string)
	c := s.anon()

	rec := c.raw("GET", "/form/"+id+"/form", nil, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(rec.Body.String(), "Team survey") || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("form page: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	for _, p := range []string{"/form/" + inactive + "/form", "/form/" + id + "/log", "/form/00000000-0000-0000-0000-000000000000/form"} {
		if rec := c.raw("GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}

	submit := func(v url.Values) *httptest.ResponseRecorder {
		return c.raw("POST", "/form/"+id+"/form", strings.NewReader(v.Encode()), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	}
	if rec := submit(url.Values{}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing required answer: %d", rec.Code)
	}
	rec = submit(url.Values{"name": {"Ada"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/form/"+id+"/form?submitted" {
		t.Fatalf("submit: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	ex := a.waitExec(rec.Header().Get("X-G2F-Execution-Id"), finished)
	if ex["status"] != "success" || ex["triggerType"] != "form" || obj(t, stepOutput(t, ex, "form")[0])["name"] != "Ada" {
		t.Fatalf("execution %v", ex)
	}
}

func TestResumeLink(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	id := a.createWF("wait", waitGraph(), nil)["id"].(string)
	execID := obj(t, a.must(http.StatusAccepted, "POST", "/api/workflows/"+id+"/run", nil))["id"].(string)
	a.waitExec(execID, waiting)
	link := strings.TrimPrefix(s.signer.URL(execID, "wait"), testPublicURL)
	if !strings.HasPrefix(link, "/webhook-resume/"+execID+"/wait/") {
		t.Fatalf("resume link %s", link)
	}
	bad := link[:len(link)-1] + map[bool]string{true: "0", false: "1"}[!strings.HasSuffix(link, "0")]
	for _, p := range []string{bad, "/webhook-resume/" + execID + "/log/" + link[strings.LastIndex(link, "/")+1:], "/webhook-resume/" + execID} {
		if rec := s.anon().raw("POST", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	if ex := obj(t, a.must(http.StatusOK, "GET", "/api/executions/"+execID, nil)); ex["status"] != "waiting" {
		t.Fatalf("bad links resumed the execution: %v", ex["status"])
	}

	rec := s.anon().raw("GET", link+"?decision=approve", nil, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	ex := a.waitExec(execID, finished)
	if ex["status"] != "success" || obj(t, obj(t, stepOutput(t, ex, "wait")[0])["query"])["decision"] != "approve" {
		t.Fatalf("resumed execution %v", ex)
	}
	if rec := s.anon().raw("GET", link, nil, nil); rec.Code != http.StatusGone {
		t.Fatalf("second resume: %d", rec.Code)
	}
}

func TestPushEndpoints(t *testing.T) {
	s := newTestServer(t)
	c := s.anon()
	rec := c.raw("POST", "/push/graph?validationToken="+url.QueryEscape("Validation: token & more"), nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "Validation: token & more" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("graph handshake: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	if rec := c.raw("POST", "/push/graph", strings.NewReader(`{"value":[{"subscriptionId":"unknown","clientState":"x"}]}`), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("graph notification: %d", rec.Code)
	}
	if rec := c.raw("POST", "/push/graph", strings.NewReader(`not json`), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed graph notification: %d", rec.Code)
	}
	if rec := c.raw("POST", "/push/gmail/unknown-token", strings.NewReader(`{"message":{"data":"e30="}}`), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown gmail token: %d", rec.Code)
	}
}

// GET /triggers describes how each trigger of the stored workflow is invoked.
func TestTriggersEndpoint(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	g := graph([]map[string]any{
		node("manual", "trigger.manual", nil),
		node("cron", "trigger.schedule", map[string]any{"cron": "0 9 * * *", "timezone": "Europe/Berlin"}),
		node("form", "trigger.form", formGraph("email")["nodes"].([]map[string]any)[0]["params"].(map[string]any)),
		node("mail", "gmail.newEmail", map[string]any{"pollMinutes": 5}),
		node("log", "core.log", nil),
	}, edge("manual", "log"))
	off := a.createWF("inactive", g, map[string]any{"scope": "group", "shares": []any{map[string]any{"group": "/sales", "role": "viewer"}}})
	byNode := func(c *client, id string) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, e := range list(t, c.must(http.StatusOK, "GET", "/api/workflows/"+id+"/triggers", nil)) {
			out[obj(t, e)["nodeId"].(string)] = obj(t, e)
		}
		return out
	}

	got := byNode(s.as(bob), off["id"].(string)) // a viewer may read them
	if len(got) != 4 {
		t.Fatalf("triggers %v", got)
	}
	if got["manual"]["listening"] != true || got["cron"]["listening"] != false || got["form"]["listening"] != false {
		t.Errorf("listening flags of an inactive workflow: %v", got)
	}
	sched := obj(t, got["cron"]["schedule"])
	if got["cron"]["kind"] != "schedule" || sched["timezone"] != "Europe/Berlin" || len(list(t, sched["next"])) != 5 {
		t.Errorf("schedule %v", got["cron"])
	}
	form := obj(t, got["form"]["form"])
	if form["url"] != testPublicURL+"/form/"+off["id"].(string)+"/form" || form["protection"] != "email" ||
		!strings.Contains(form["warning"].(string), "E-mail is not configured") {
		t.Errorf("form %v", form)
	}
	if poll := obj(t, got["mail"]["poll"]); got["mail"]["kind"] != "poll" || poll["configuredMinutes"] == nil {
		t.Errorf("poll %v", got["mail"])
	}

	s.as(orgAdmin).must(http.StatusOK, "PUT", "/api/plugins/email/config", map[string]any{"values": map[string]any{"smtpHost": "smtp.acme.test"}})
	on := a.createWF("active", g, map[string]any{"active": true})
	got = byNode(a, on["id"].(string))
	if got["cron"]["listening"] != true || got["form"]["listening"] != true || obj(t, got["form"]["form"])["warning"] != nil {
		t.Errorf("active workflow with e-mail configured: %v", got)
	}
}

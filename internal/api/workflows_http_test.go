package api

import (
	"net/http"
	"strings"
	"testing"
)

// erin is a member of the parent group /eng only.
var erin = testUser{Sub: "erin", Name: "Erin", Org: "acme", Groups: []string{"/eng"}}

// expect lists the status codes a user gets for the workflow operations.
type expect struct {
	get, run, edit, share, del int
}

var (
	noAccess = expect{http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound, http.StatusNotFound}
	asViewer = expect{http.StatusOK, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden}
	asRunner = expect{http.StatusOK, http.StatusAccepted, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden}
	asEditor = expect{http.StatusOK, http.StatusAccepted, http.StatusOK, http.StatusForbidden, http.StatusForbidden}
	asOwner  = expect{http.StatusOK, http.StatusAccepted, http.StatusOK, http.StatusOK, http.StatusNoContent}
)

// checkAccess performs every workflow operation as u and compares the status
// codes with want. Refused writes must leave the stored workflow unchanged.
// The owner client reads the stored state.
func checkAccess(t *testing.T, s *testServer, owner *client, u testUser, wfID string, want expect) {
	t.Helper()
	c := s.as(u)
	base := "/api/workflows/" + wfID
	stored := func() map[string]any { return obj(t, owner.must(http.StatusOK, "GET", base, nil)) }
	unchanged := func(op string, before map[string]any) {
		t.Helper()
		after := stored()
		if after["version"] != before["version"] || after["name"] != before["name"] ||
			mustJSON(t, after["shares"]) != mustJSON(t, before["shares"]) || after["scope"] != before["scope"] {
			t.Errorf("%s: refused write changed the workflow: before %v, after %v", op, before, after)
		}
	}

	if got, body := c.do("GET", base, nil); got != want.get {
		t.Errorf("GET: %d, want %d (%v)", got, want.get, body)
	} else if got == http.StatusNotFound && errorOf(t, body) != "not found" {
		t.Errorf("GET 404 must not reveal anything: %v", body)
	}
	if got, _ := c.do("GET", base+"/executions", nil); got != want.get {
		t.Errorf("GET executions: %d, want %d", got, want.get)
	}
	if got, _ := c.do("GET", base+"/triggers", nil); got != want.get {
		t.Errorf("GET triggers: %d, want %d", got, want.get)
	}
	if got, body := c.do("POST", base+"/run", nil); got != want.run {
		t.Errorf("run: %d, want %d (%v)", got, want.run, body)
	}

	before := stored()
	got, body := c.do("PUT", base, update(before, map[string]any{"name": "renamed by " + u.Sub}))
	if got != want.edit {
		t.Errorf("edit: %d, want %d (%v)", got, want.edit, body)
	}
	if got != http.StatusOK {
		unchanged("edit", before)
	}

	before = stored()
	got, body = c.do("PUT", base, update(before, map[string]any{
		"scope": "group", "shares": []any{map[string]any{"group": "/sales", "role": "editor"}, map[string]any{"group": "/eng", "role": "editor"}},
	}))
	if got != want.share {
		t.Errorf("change sharing: %d, want %d (%v)", got, want.share, body)
	}
	if got != http.StatusOK {
		unchanged("change sharing", before)
		// Editors are told that sharing is owner-only; lower roles cannot edit at all.
		if got == http.StatusForbidden {
			if msg := errorOf(t, body); !strings.Contains(msg, "only the owner") && msg != "requires role editor" {
				t.Errorf("unexpected message: %v", body)
			}
		}
	}

	before = stored()
	if got, body := c.do("DELETE", base, nil); got != want.del {
		t.Errorf("delete: %d, want %d (%v)", got, want.del, body)
	} else if got == http.StatusNoContent {
		owner.must(http.StatusNotFound, "GET", base, nil)
	} else {
		unchanged("delete", before)
	}
}

func TestSharingRoles(t *testing.T) {
	s := newTestServer(t)
	owner := s.as(alice)

	cases := []struct {
		name   string
		create map[string]any
		user   testUser
		want   expect
	}{
		{"private workflow is invisible to others in the org", nil, dave, noAccess},
		{"private workflow: other org gets 404", nil, carol, noAccess},
		{"group viewer can read but not run, edit or delete", shared("/eng", "viewer"), dave, asViewer},
		{"group runner can run but not edit", shared("/eng", "runner"), dave, asRunner},
		{"group editor can edit but not change sharing or delete", shared("/eng", "editor"), dave, asEditor},
		{"member of the parent group does not get a subgroup's share", shared("/eng/backend", "editor"), erin, noAccess},
		{"member of another group gets 404", shared("/eng", "editor"), bob, noAccess},
		{"same group name in another org gets 404", shared("/eng", "editor"), carol, noAccess},
		{"org scope gives everyone in the org the org role", map[string]any{"scope": "org", "orgRole": "runner"}, bob, asRunner},
		{"org scope without orgRole means viewer", map[string]any{"scope": "org"}, bob, asViewer},
		{"org scope plus group share grants the higher role", map[string]any{"scope": "org", "orgRole": "viewer",
			"shares": []any{map[string]any{"group": "/eng", "role": "editor"}}}, dave, asEditor},
		{"org scope is not visible in other orgs", map[string]any{"scope": "org", "orgRole": "editor"}, carol, noAccess},
		{"other org's admin gets 404", map[string]any{"scope": "org", "orgRole": "editor"}, otherAdmin, noAccess},
		{"org admin is owner of every workflow of the org", nil, orgAdmin, asOwner},
		{"owner can do everything", nil, alice, asOwner},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := owner.createWF("wf "+c.name, manualGraph(), c.create)
			checkAccess(t, s, owner, c.user, wf["id"].(string), c.want)
		})
	}
}

func TestListShowsOnlyAccessibleWorkflows(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	private := a.createWF("private", manualGraph(), nil)["id"].(string)
	group := a.createWF("group", manualGraph(), shared("/eng", "runner"))["id"].(string)
	subgroup := a.createWF("subgroup", manualGraph(), shared("/eng/backend", "viewer"))["id"].(string)
	org := a.createWF("org", manualGraph(), map[string]any{"scope": "org"})["id"].(string)
	carols := s.as(carol).createWF("carol's", manualGraph(), map[string]any{"scope": "org", "orgRole": "editor"})["id"].(string)

	cases := []struct {
		user          testUser
		want, notWant []string
	}{
		{alice, []string{private, group, subgroup, org}, []string{carols}},
		{dave, []string{group, org}, []string{private, subgroup, carols}},
		{erin, []string{group, org}, []string{private, subgroup, carols}},
		{bob, []string{org}, []string{private, group, subgroup, carols}},
		{orgAdmin, []string{private, group, subgroup, org}, []string{carols}},
		{carol, []string{carols}, []string{private, group, subgroup, org}},
		{otherAdmin, []string{carols}, []string{private, group, subgroup, org}},
	}
	for _, c := range cases {
		t.Run(c.user.Sub, func(t *testing.T) {
			got := ids(t, s.as(c.user).must(http.StatusOK, "GET", "/api/workflows", nil))
			for _, id := range c.want {
				if !contains(got, id) {
					t.Errorf("missing %s", id)
				}
			}
			for _, id := range c.notWant {
				if contains(got, id) {
					t.Errorf("BUG: %s sees %s", c.user.Sub, id)
				}
			}
		})
	}

	t.Run("summaries carry the caller's access and triggers but no graph", func(t *testing.T) {
		for _, e := range list(t, s.as(dave).must(http.StatusOK, "GET", "/api/workflows", nil)) {
			w := obj(t, e)
			if _, ok := w["graph"]; ok {
				t.Errorf("summary contains the graph: %v", w)
			}
			if mustJSON(t, w["triggers"]) != `["manual"]` {
				t.Errorf("triggers %v", w["triggers"])
			}
			want := map[string]string{group: "runner", org: "viewer"}[w["id"].(string)]
			if w["access"] != want {
				t.Errorf("%s: access %v, want %s", w["name"], w["access"], want)
			}
		}
	})
}

func TestEditorRules(t *testing.T) {
	s := newTestServer(t)
	owner := s.as(alice)
	ed := s.as(dave)

	t.Run("editor can activate and deactivate", func(t *testing.T) {
		wf := owner.createWF("activate", manualGraph(), shared("/eng", "editor"))
		got := obj(t, ed.must(http.StatusOK, "PUT", "/api/workflows/"+wf["id"].(string), update(wf, map[string]any{"active": true})))
		if got["active"] != true || got["access"] != "editor" {
			t.Fatalf("activate: %v", got)
		}
		got = obj(t, ed.must(http.StatusOK, "PUT", "/api/workflows/"+wf["id"].(string), update(got, map[string]any{"active": false})))
		if got["active"] != false {
			t.Fatalf("deactivate: %v", got)
		}
	})
	t.Run("runner cannot activate", func(t *testing.T) {
		wf := owner.createWF("runner", manualGraph(), shared("/eng", "runner"))
		ed.must(http.StatusForbidden, "PUT", "/api/workflows/"+wf["id"].(string), update(wf, map[string]any{"active": true}))
		if obj(t, owner.must(http.StatusOK, "GET", "/api/workflows/"+wf["id"].(string), nil))["active"] != false {
			t.Fatal("refused activation changed the workflow")
		}
	})
	t.Run("editor may resend sharing in an equivalent spelling", func(t *testing.T) {
		wf := owner.createWF("spelling", manualGraph(), shared("/eng", "editor"))
		ed.must(http.StatusOK, "PUT", "/api/workflows/"+wf["id"].(string), update(wf, map[string]any{
			"shares": []any{map[string]any{"group": "eng/", "role": "editor"}},
		}))
	})
	t.Run("editor cannot raise the org role", func(t *testing.T) {
		wf := owner.createWF("orgrole", manualGraph(), map[string]any{"scope": "org", "orgRole": "viewer",
			"shares": []any{map[string]any{"group": "/eng", "role": "editor"}}})
		ed.must(http.StatusForbidden, "PUT", "/api/workflows/"+wf["id"].(string), update(wf, map[string]any{"orgRole": "editor"}))
	})
	t.Run("owner can make a shared workflow private again", func(t *testing.T) {
		wf := owner.createWF("unshare", manualGraph(), shared("/eng", "editor"))
		ed.must(http.StatusOK, "GET", "/api/workflows/"+wf["id"].(string), nil)
		owner.must(http.StatusOK, "PUT", "/api/workflows/"+wf["id"].(string), update(wf, map[string]any{"scope": "private", "shares": []any{}}))
		ed.must(http.StatusNotFound, "GET", "/api/workflows/"+wf["id"].(string), nil)
	})
}

func TestTenantIsolation(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	// As open as possible inside acme, and shared with group names carol has too.
	wf := a.createWF("acme", waitGraph(), map[string]any{"scope": "org", "orgRole": "editor",
		"shares": []any{map[string]any{"group": "/eng/backend", "role": "editor"}}})
	id := wf["id"].(string)
	exec := obj(t, a.must(http.StatusAccepted, "POST", "/api/workflows/"+id+"/run", nil))
	execID := exec["id"].(string)
	a.waitExec(execID, waiting)

	for _, u := range []testUser{carol, otherAdmin} {
		c := s.as(u)
		for _, r := range []struct{ method, path string }{
			{"GET", "/api/workflows/" + id},
			{"PUT", "/api/workflows/" + id},
			{"DELETE", "/api/workflows/" + id},
			{"POST", "/api/workflows/" + id + "/run"},
			{"GET", "/api/workflows/" + id + "/executions"},
			{"GET", "/api/workflows/" + id + "/triggers"},
			{"GET", "/api/executions/" + execID},
			{"POST", "/api/executions/" + execID + "/cancel"},
		} {
			var body any
			if r.method == "PUT" {
				body = update(wf, map[string]any{"name": "hijacked"})
			}
			if got, resp := c.do(r.method, r.path, body); got != http.StatusNotFound {
				t.Errorf("%s %s %s: %d, want 404 (%v)", u.Sub, r.method, r.path, got, resp)
			}
		}
	}
	// Nothing changed: still waiting, same name and version.
	if got := obj(t, a.must(http.StatusOK, "GET", "/api/workflows/"+id, nil)); got["name"] != "acme" || got["version"] != float64(1) {
		t.Fatalf("workflow changed: %v", got)
	}
	if got := obj(t, a.must(http.StatusOK, "GET", "/api/executions/"+execID, nil)); got["status"] != "waiting" {
		t.Fatalf("execution changed: %v", got["status"])
	}

	t.Run("create ignores org, owner, id and version from the body", func(t *testing.T) {
		body := wfBody("forged", manualGraph(), map[string]any{
			"orgId": "other", "ownerSub": "carol", "ownerName": "Carol", "id": "00000000-0000-0000-0000-0000000000aa",
			"version": 42, "access": "owner",
		})
		got := obj(t, a.must(http.StatusCreated, "POST", "/api/workflows", body))
		if got["orgId"] != "acme" || got["ownerSub"] != "alice" || got["ownerName"] != "Alice" ||
			got["id"] == "00000000-0000-0000-0000-0000000000aa" || got["version"] != float64(1) {
			t.Fatalf("caller chose org/owner/id/version: %v", got)
		}
		s.as(carol).must(http.StatusNotFound, "GET", "/api/workflows/"+got["id"].(string), nil)
	})
	t.Run("update ignores org and owner from the body", func(t *testing.T) {
		got := obj(t, a.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(wf, map[string]any{"orgId": "other", "ownerSub": "carol"})))
		if got["orgId"] != "acme" || got["ownerSub"] != "alice" {
			t.Fatalf("update moved the workflow: %v", got)
		}
	})
}

func TestWorkflowCRUD(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)

	wf := a.createWF("crud", manualGraph(), map[string]any{"description": "d"})
	id := wf["id"].(string)
	if wf["orgId"] != "acme" || wf["ownerSub"] != "alice" || wf["version"] != float64(1) || wf["access"] != "owner" ||
		wf["scope"] != "private" || wf["orgRole"] != "viewer" || mustJSON(t, wf["shares"]) != "[]" || wf["active"] != false {
		t.Fatalf("created: %v", wf)
	}
	got := obj(t, a.must(http.StatusOK, "GET", "/api/workflows/"+id, nil))
	if got["name"] != "crud" || got["description"] != "d" || len(list(t, obj(t, got["graph"])["nodes"])) != 2 {
		t.Fatalf("get: %v", got)
	}
	// Edge handles are filled in on save.
	e := obj(t, list(t, obj(t, got["graph"])["edges"])[0])
	if e["sourceHandle"] != "main" || e["targetHandle"] != "main" {
		t.Errorf("edge handles not normalized: %v", e)
	}

	v2 := obj(t, a.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(got, map[string]any{"name": "  renamed  "})))
	if v2["name"] != "renamed" || v2["version"] != float64(2) {
		t.Fatalf("update: %v", v2)
	}

	t.Run("stale version is a conflict and changes nothing", func(t *testing.T) {
		code, body := a.do("PUT", "/api/workflows/"+id, update(got, map[string]any{"name": "stale"})) // version 1
		if code != http.StatusConflict || !strings.Contains(errorOf(t, body), "modified by someone else") {
			t.Fatalf("stale update: %d %v", code, body)
		}
		now := obj(t, a.must(http.StatusOK, "GET", "/api/workflows/"+id, nil))
		if now["name"] != "renamed" || now["version"] != float64(2) {
			t.Fatalf("stored workflow changed: %v", now)
		}
	})
	t.Run("invalid update is rejected and changes nothing", func(t *testing.T) {
		code, body := a.do("PUT", "/api/workflows/"+id, update(v2, map[string]any{"graph": graph([]map[string]any{node("x", "no.such", nil)})}))
		if code != http.StatusBadRequest {
			t.Fatalf("%d %v", code, body)
		}
		now := obj(t, a.must(http.StatusOK, "GET", "/api/workflows/"+id, nil))
		if now["version"] != float64(2) || len(list(t, obj(t, now["graph"])["nodes"])) != 2 {
			t.Fatalf("stored workflow changed: %v", now)
		}
	})
	t.Run("unknown and malformed ids are 404", func(t *testing.T) {
		for _, p := range []string{"/api/workflows/00000000-0000-0000-0000-000000000000", "/api/workflows/not-a-uuid"} {
			if code, body := a.do("GET", p, nil); code != http.StatusNotFound || errorOf(t, body) != "not found" {
				t.Errorf("%s: %d %v", p, code, body)
			}
		}
	})

	a.must(http.StatusNoContent, "DELETE", "/api/workflows/"+id, nil)
	a.must(http.StatusNotFound, "GET", "/api/workflows/"+id, nil)
	a.must(http.StatusNotFound, "DELETE", "/api/workflows/"+id, nil)
	if contains(ids(t, a.must(http.StatusOK, "GET", "/api/workflows", nil)), id) {
		t.Fatal("deleted workflow still listed")
	}
}

func TestWorkflowValidation(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	set := func(fields any) map[string]any {
		return graph([]map[string]any{node("start", "trigger.manual", nil), node("set", "core.set", map[string]any{"fields": fields})},
			edge("start", "set"))
	}
	cases := []struct {
		name string
		body any
		want string
	}{
		{"empty name", wfBody("  ", manualGraph(), nil), "name is required"},
		{"unknown node type", wfBody("x", graph([]map[string]any{node("a", "no.such.type", nil)}), nil), `unknown type "no.such.type"`},
		{"edge to a missing node", wfBody("x", graph([]map[string]any{node("a", "trigger.manual", nil)}, edge("a", "ghost")), nil), "references unknown node"},
		{"duplicate node id", wfBody("x", graph([]map[string]any{node("a", "trigger.manual", nil), node("a", "core.set", nil)}), nil), "duplicate node id"},
		{"node without id", wfBody("x", graph([]map[string]any{node("", "trigger.manual", nil)}), nil), "node without id"},
		{"cycle", wfBody("x", graph([]map[string]any{node("a", "core.set", nil), node("b", "core.set", nil)}, edge("a", "b"), edge("b", "a")), nil), "cycle"},
		{"edge into a trigger", wfBody("x", graph([]map[string]any{node("a", "core.set", nil), node("t", "trigger.manual", nil)}, edge("a", "t")), nil), "cannot have incoming"},
		{"unknown output handle", wfBody("x", graph([]map[string]any{node("a", "trigger.manual", nil), node("b", "core.set", nil)},
			map[string]any{"id": "e", "source": "a", "target": "b", "sourceHandle": "nope"}), nil), `has no output "nope"`},
		{"invalid cron", wfBody("x", graph([]map[string]any{node("cron", "trigger.schedule", map[string]any{"cron": "every tuesday"})}), nil), "invalid cron expression"},
		{"sub-minute schedule", wfBody("x", graph([]map[string]any{node("cron", "trigger.schedule", map[string]any{"cron": "@every 10s"})}), nil), "more often than"},
		{"webhook auth without secret", wfBody("x", graph([]map[string]any{node("hook", "trigger.webhook", map[string]any{"auth": "token"})}), nil), "secret is required"},
		{"field reads plugin config", wfBody("x", set(map[string]any{"t": "{{ config.token }}"}), nil), "config.* is only available to the plugin"},
		{"field reads a credential", wfBody("x", set(map[string]any{"t": "{{ credential.token }}"}), nil), "credential.* is only available to the plugin"},
		{"expression syntax error", wfBody("x", set(map[string]any{"t": "{{ 1 + }}"}), nil), "expression"},
		{"invalid scope", wfBody("x", manualGraph(), map[string]any{"scope": "public"}), `invalid scope "public"`},
		{"owner is not a shareable role", wfBody("x", manualGraph(), shared("/eng", "owner")), `invalid role "owner"`},
		{"empty share group", wfBody("x", manualGraph(), shared(" ", "viewer")), "share group must not be empty"},
		{"duplicate share", wfBody("x", manualGraph(), map[string]any{"scope": "group",
			"shares": []any{map[string]any{"group": "/eng", "role": "viewer"}, map[string]any{"group": "eng", "role": "editor"}}}), "duplicate share"},
		{"invalid orgRole", wfBody("x", manualGraph(), map[string]any{"scope": "org", "orgRole": "admin"}), `invalid orgRole "admin"`},
		{"secret parameter that is not a string", wfBody("x", graph([]map[string]any{node("hook", "trigger.webhook",
			map[string]any{"auth": "none", "secret": 12345})}), nil), "must be a string"},
		{"invalid JSON", "{", "invalid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var code int
			var body any
			if raw, ok := c.body.(string); ok {
				rec := a.raw("POST", "/api/workflows", strings.NewReader(raw), http.Header{"Content-Type": {"application/json"}})
				code, body = rec.Code, decodeBody(t, rec)
			} else {
				code, body = a.do("POST", "/api/workflows", c.body)
			}
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %v", code, body)
			}
			if msg := errorOf(t, body); !strings.Contains(msg, c.want) {
				t.Fatalf("error %q does not mention %q", msg, c.want)
			}
		})
	}
	if n := len(list(t, a.must(http.StatusOK, "GET", "/api/workflows", nil))); n != 0 {
		t.Fatalf("rejected workflows were stored: %d", n)
	}

	t.Run("reserved __ names are rejected on save", func(t *testing.T) {
		for _, code := range []string{"{{ __budget }}", "{{ let __x = 1; __x }}", "{{ __tick(1) }}", "{{ json.a + __ctx }}"} {
			c, body := a.do("POST", "/api/workflows", wfBody("reserved", set(map[string]any{"t": code}), nil))
			if c != http.StatusBadRequest || !strings.Contains(errorOf(t, body), `names starting with "__" are reserved`) {
				t.Errorf("%s: %d %v", code, c, body)
			}
		}
	})
	t.Run("fields named with underscores are allowed", func(t *testing.T) {
		a.must(http.StatusCreated, "POST", "/api/workflows", wfBody("typename",
			set(map[string]any{"t": `{{ json.__typename }} {{ json["__x"] }}`}), nil))
	})
}

// Execute Workflow needs the runner role on its target (checkWorkflowRefs).
func TestWorkflowReferencesNeedAccess(t *testing.T) {
	s := newTestServer(t)
	a, b := s.as(alice), s.as(bob)
	target := a.createWF("target", manualGraph(), nil)
	targetID := target["id"].(string)
	caller := func() map[string]any {
		return graph([]map[string]any{
			node("start", "trigger.manual", nil),
			node("call", "flow.executeWorkflow", map[string]any{"workflow": targetID}),
		}, edge("start", "call"))
	}

	a.must(http.StatusCreated, "POST", "/api/workflows", wfBody("own target", caller(), nil))

	code, body := b.do("POST", "/api/workflows", wfBody("bob calls private", caller(), nil))
	if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "not shared with you as runner") {
		t.Fatalf("private target: %d %v", code, body)
	}
	target = obj(t, a.must(http.StatusOK, "PUT", "/api/workflows/"+targetID, update(target, shared("/sales", "viewer"))))
	if code, body := b.do("POST", "/api/workflows", wfBody("bob calls as viewer", caller(), nil)); code != http.StatusBadRequest {
		t.Fatalf("viewer may not call a workflow: %d %v", code, body)
	}
	a.must(http.StatusOK, "PUT", "/api/workflows/"+targetID, update(target, shared("/sales", "runner")))
	b.must(http.StatusCreated, "POST", "/api/workflows", wfBody("bob calls as runner", caller(), nil))

	if code, _ := s.as(carol).do("POST", "/api/workflows", wfBody("carol", caller(), nil)); code != http.StatusBadRequest {
		t.Fatalf("other org may reference the workflow: %d", code)
	}
}

func TestRunsAndExecutions(t *testing.T) {
	s := newTestServer(t)
	owner := s.as(alice)
	runner, viewer := s.as(dave), s.as(erin) // shares below: /eng/frontend runner, /eng viewer

	wf := owner.createWF("runs", manualGraph(), map[string]any{"scope": "group", "shares": []any{
		map[string]any{"group": "/eng", "role": "viewer"}, map[string]any{"group": "/eng/frontend", "role": "runner"},
	}})
	id := wf["id"].(string)

	t.Run("runner starts an execution that finishes with steps", func(t *testing.T) {
		started := obj(t, runner.must(http.StatusAccepted, "POST", "/api/workflows/"+id+"/run", nil))
		if started["workflowId"] != id || started["triggerNode"] != "start" || started["triggeredBy"] != "Dave" {
			t.Fatalf("started: %v", started)
		}
		ex := runner.waitExec(started["id"].(string), finished)
		if ex["status"] != "success" || len(list(t, ex["steps"])) != 2 {
			t.Fatalf("execution: %v", ex)
		}
		if out := stepOutput(t, ex, "set"); mustJSON(t, out) != `[{"hello":"world"}]` {
			t.Fatalf("set output %v", out)
		}
		// Viewers see executions; others do not.
		got := ids(t, viewer.must(http.StatusOK, "GET", "/api/workflows/"+id+"/executions", nil))
		if !contains(got, started["id"].(string)) {
			t.Fatalf("viewer's execution list %v", got)
		}
		viewer.must(http.StatusOK, "GET", "/api/executions/"+started["id"].(string), nil)
		for _, u := range []testUser{bob, carol} {
			s.as(u).must(http.StatusNotFound, "GET", "/api/executions/"+started["id"].(string), nil)
			s.as(u).must(http.StatusNotFound, "GET", "/api/workflows/"+id+"/executions", nil)
		}
	})
	t.Run("viewer cannot run", func(t *testing.T) {
		code, body := viewer.do("POST", "/api/workflows/"+id+"/run", nil)
		if code != http.StatusForbidden || errorOf(t, body) != "requires role runner" {
			t.Fatalf("%d %v", code, body)
		}
	})
	t.Run("run from a node that is not a trigger is 400", func(t *testing.T) {
		code, body := runner.do("POST", "/api/workflows/"+id+"/run", map[string]any{"nodeId": "set"})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "is not a trigger") {
			t.Fatalf("%d %v", code, body)
		}
		code, body = runner.do("POST", "/api/workflows/"+id+"/run", map[string]any{"nodeId": "ghost"})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "not found") {
			t.Fatalf("%d %v", code, body)
		}
		rec := runner.raw("POST", "/api/workflows/"+id+"/run", strings.NewReader("{nope"), http.Header{"Content-Type": {"application/json"}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid JSON: %d", rec.Code)
		}
	})
	t.Run("unknown execution ids are 404", func(t *testing.T) {
		for _, p := range []string{"/api/executions/00000000-0000-0000-0000-000000000000", "/api/executions/nope"} {
			owner.must(http.StatusNotFound, "GET", p, nil)
			owner.must(http.StatusNotFound, "POST", p+"/cancel", nil)
		}
	})
	t.Run("list limit is bounded", func(t *testing.T) {
		for _, q := range []string{"?limit=1", "?limit=-5", "?limit=100000", "?limit=x"} {
			l := list(t, viewer.must(http.StatusOK, "GET", "/api/workflows/"+id+"/executions"+q, nil))
			if q == "?limit=1" && len(l) != 1 {
				t.Errorf("limit=1 returned %d", len(l))
			}
		}
	})
}

func TestCancelWaitingExecution(t *testing.T) {
	s := newTestServer(t)
	owner := s.as(alice)
	runner, viewer := s.as(dave), s.as(erin)
	wf := owner.createWF("wait", waitGraph(), map[string]any{"scope": "group", "shares": []any{
		map[string]any{"group": "/eng", "role": "viewer"}, map[string]any{"group": "/eng/frontend", "role": "runner"},
	}})
	id := wf["id"].(string)
	execID := obj(t, owner.must(http.StatusAccepted, "POST", "/api/workflows/"+id+"/run", nil))["id"].(string)
	if ex := owner.waitExec(execID, waiting); ex["status"] != "waiting" {
		t.Fatalf("not waiting: %v", ex)
	}

	viewer.must(http.StatusForbidden, "POST", "/api/executions/"+execID+"/cancel", nil)
	s.as(bob).must(http.StatusNotFound, "POST", "/api/executions/"+execID+"/cancel", nil)
	s.as(carol).must(http.StatusNotFound, "POST", "/api/executions/"+execID+"/cancel", nil)
	if ex := obj(t, owner.must(http.StatusOK, "GET", "/api/executions/"+execID, nil)); ex["status"] != "waiting" {
		t.Fatalf("refused cancel changed the execution: %v", ex["status"])
	}

	runner.must(http.StatusNoContent, "POST", "/api/executions/"+execID+"/cancel", nil)
	ex := obj(t, owner.must(http.StatusOK, "GET", "/api/executions/"+execID, nil))
	if ex["status"] != "error" || ex["error"] != "cancelled by Dave" {
		t.Fatalf("cancelled: %v", ex)
	}
	code, body := runner.do("POST", "/api/executions/"+execID+"/cancel", nil)
	if code != http.StatusConflict || errorOf(t, body) != "execution is not waiting" {
		t.Fatalf("second cancel: %d %v", code, body)
	}
}

// Items stand in for trigger data: runners may pass them to the Manual
// trigger only, editors to any trigger.
func TestSupplyingTriggerItems(t *testing.T) {
	s := newTestServer(t)
	owner := s.as(alice)
	g := graph([]map[string]any{
		node("start", "trigger.manual", nil),
		node("hook", "trigger.webhook", map[string]any{"auth": "token", "secret": "hook-secret-value"}),
		formGraph("none")["nodes"].([]map[string]any)[0],
		node("log", "core.log", map[string]any{"message": "{{ json.body.approved }}"}),
	}, edge("hook", "log"))
	runnerWF := owner.createWF("forge", g, shared("/eng/frontend", "runner"))["id"].(string)
	editorWF := owner.createWF("forge-editor", g, shared("/eng/frontend", "editor"))["id"].(string)
	// Only a webhook trigger: the default trigger of a manual run.
	hookOnly := owner.createWF("hook only", graph([]map[string]any{
		node("hook", "trigger.webhook", map[string]any{"auth": "token", "secret": "hook-secret-value"}),
		node("log", "core.log", nil),
	}, edge("hook", "log")), shared("/eng/frontend", "runner"))["id"].(string)

	forged := []any{map[string]any{"method": "POST", "body": map[string]any{"approved": true}, "submittedBy": "boss"}}
	execCount := func(id string) int {
		return len(list(t, owner.must(http.StatusOK, "GET", "/api/workflows/"+id+"/executions", nil)))
	}
	run := func(who testUser, id string, body map[string]any) (int, any) {
		return s.as(who).do("POST", "/api/workflows/"+id+"/run", body)
	}

	t.Run("runner with items on the manual trigger", func(t *testing.T) {
		code, body := run(dave, runnerWF, map[string]any{"nodeId": "start", "items": []any{map[string]any{"n": 1}}})
		if code != http.StatusAccepted {
			t.Fatalf("%d %v", code, body)
		}
		s.as(dave).waitExec(obj(t, body)["id"].(string), finished)
	})
	for id, kind := range map[string]string{"hook": "webhook", "form": "form"} {
		t.Run("runner with items on the "+kind+" trigger is refused", func(t *testing.T) {
			before := execCount(runnerWF)
			code, body := run(dave, runnerWF, map[string]any{"nodeId": id, "items": forged})
			if code != http.StatusForbidden || !strings.Contains(errorOf(t, body), "only editors can supply items for a "+kind+" trigger") {
				t.Fatalf("%d %v", code, body)
			}
			if n := execCount(runnerWF); n != before {
				t.Fatalf("executions: %d, want %d", n, before)
			}
		})
	}
	t.Run("runner with items and no nodeId, default trigger is a webhook", func(t *testing.T) {
		code, body := run(dave, hookOnly, map[string]any{"items": forged})
		if code != http.StatusForbidden || !strings.Contains(errorOf(t, body), "webhook trigger") {
			t.Fatalf("%d %v", code, body)
		}
		if n := execCount(hookOnly); n != 0 {
			t.Fatalf("%d executions", n)
		}
	})
	t.Run("runner without items on a webhook trigger", func(t *testing.T) {
		code, body := run(dave, runnerWF, map[string]any{"nodeId": "hook"})
		if code != http.StatusAccepted {
			t.Fatalf("%d %v", code, body)
		}
		s.as(dave).waitExec(obj(t, body)["id"].(string), finished)
		if code, body := run(dave, hookOnly, nil); code != http.StatusAccepted {
			t.Fatalf("no body: %d %v", code, body)
		}
	})
	t.Run("runner with items for an unknown node is a bad request", func(t *testing.T) {
		if code, body := run(dave, runnerWF, map[string]any{"nodeId": "ghost", "items": forged}); code != http.StatusBadRequest {
			t.Fatalf("%d %v", code, body)
		}
	})
	for _, id := range []string{"hook", "form"} {
		t.Run("editor with items on the "+id+" trigger", func(t *testing.T) {
			code, body := run(dave, editorWF, map[string]any{"nodeId": id, "items": forged})
			if code != http.StatusAccepted {
				t.Fatalf("%d %v", code, body)
			}
			ex := s.as(dave).waitExec(obj(t, body)["id"].(string), finished)
			if ex["triggerNode"] != id || ex["triggerType"] != "manual" {
				t.Fatalf("execution: %v", ex)
			}
		})
	}
}

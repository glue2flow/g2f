package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var secretRef = map[string]any{"$secret": "stored"}

// hookGraph is an active webhook workflow with token authentication.
func hookGraph(secret any, auth string) map[string]any {
	params := map[string]any{"auth": auth, "testBody": map[string]any{"from": "editor"}}
	if secret != "<omit>" {
		params["secret"] = secret
	}
	return graph([]map[string]any{
		node("hook", "trigger.webhook", params),
		node("log", "core.log", map[string]any{"message": "got {{ json.body }}"}),
	}, edge("hook", "log"))
}

func hookParams(t *testing.T, wf map[string]any) map[string]any {
	t.Helper()
	for _, n := range list(t, obj(t, wf["graph"])["nodes"]) {
		if obj(t, n)["id"] == "hook" {
			return obj(t, obj(t, n)["params"])
		}
	}
	t.Fatal("no hook node")
	return nil
}

// callHook calls the public webhook endpoint (no bearer token) with token.
func callHook(s *testServer, wfID, token string) *httptest.ResponseRecorder {
	hdr := http.Header{"Content-Type": {"application/json"}}
	if token != "" {
		hdr.Set("X-G2F-Token", token)
	}
	return s.anon().raw("POST", "/webhook/"+wfID+"/hook", strings.NewReader(`{"n":1}`), hdr)
}

func TestNodeSecretsNeverLeave(t *testing.T) {
	const first, second = "hook-secret-FIRST-value", "hook-secret-SECOND-value"
	s := newTestServer(t)
	owner, viewer := s.as(alice), s.as(dave)

	createRec := owner.send("POST", "/api/workflows", wfBody("secret hook", hookGraph(first, "token"),
		map[string]any{"active": true, "scope": "group", "shares": []any{map[string]any{"group": "/eng", "role": "viewer"}}}))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", createRec.Code, createRec.Body)
	}
	wf := obj(t, decodeBody(t, createRec))
	id := wf["id"].(string)
	if p := hookParams(t, wf); mustJSON(t, p["secret"]) != mustJSON(t, secretRef) {
		t.Fatalf("create response secret = %v, want the stored reference", p["secret"])
	}

	// A webhook call and a manual run from the webhook trigger produce executions.
	if r := callHook(s, id, first); r.Code != http.StatusAccepted {
		t.Fatalf("webhook with the right token: %d %s", r.Code, r.Body)
	}
	manual := obj(t, owner.must(http.StatusAccepted, "POST", "/api/workflows/"+id+"/run", map[string]any{"nodeId": "hook"}))
	owner.waitExec(manual["id"].(string), finished)
	var execs []any
	eventually(t, "two finished executions", func() bool {
		execs = list(t, owner.must(http.StatusOK, "GET", "/api/workflows/"+id+"/executions", nil))
		if len(execs) != 2 {
			return false
		}
		for _, e := range execs {
			if !finished(obj(t, e)["status"].(string)) {
				return false
			}
		}
		return true
	})

	bodies := map[string]string{
		"create response":     createRec.Body.String(),
		"owner GET":           owner.send("GET", "/api/workflows/"+id, nil).Body.String(),
		"viewer GET":          viewer.send("GET", "/api/workflows/"+id, nil).Body.String(),
		"owner list":          owner.send("GET", "/api/workflows", nil).Body.String(),
		"viewer list":         viewer.send("GET", "/api/workflows", nil).Body.String(),
		"triggers":            owner.send("GET", "/api/workflows/"+id+"/triggers", nil).Body.String(),
		"executions list":     owner.send("GET", "/api/workflows/"+id+"/executions", nil).Body.String(),
		"nodes":               owner.send("GET", "/api/nodes", nil).Body.String(),
		"webhook response":    callHook(s, id, first).Body.String(),
		"webhook 401 for bob": callHook(s, id, "wrong").Body.String(),
	}
	for _, e := range execs {
		eid := obj(t, e)["id"].(string)
		bodies["execution "+eid] = owner.send("GET", "/api/executions/"+eid, nil).Body.String()
	}
	assertNoLeak(t, first, bodies)
	if !strings.Contains(bodies["viewer GET"], `"secret":{"$secret":"stored"}`) {
		t.Errorf("viewer GET does not mark the stored secret: %s", bodies["viewer GET"])
	}

	trig := obj(t, list(t, owner.must(http.StatusOK, "GET", "/api/workflows/"+id+"/triggers", nil))[0])
	hook := obj(t, trig["webhook"])
	if trig["kind"] != "webhook" || trig["listening"] != true || trig["error"] != nil ||
		hook["url"] != testPublicURL+"/webhook/"+id+"/hook" || hook["auth"] != "token" || hook["header"] != "X-G2F-Token" {
		t.Fatalf("triggers: %v", trig)
	}

	stored := func() map[string]any { return obj(t, owner.must(http.StatusOK, "GET", "/api/workflows/"+id, nil)) }

	t.Run("saving the reference keeps the secret", func(t *testing.T) {
		cur := stored()
		owner.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(cur, map[string]any{"name": "renamed"}))
		if r := callHook(s, id, first); r.Code != http.StatusAccepted {
			t.Fatalf("old secret no longer accepted: %d %s", r.Code, r.Body)
		}
	})
	t.Run("an editor saving the reference keeps the secret too", func(t *testing.T) {
		owner.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(stored(), map[string]any{
			"shares": []any{map[string]any{"group": "/eng", "role": "editor"}}}))
		viewer.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(stored(), map[string]any{"description": "edited by dave"}))
		if r := callHook(s, id, first); r.Code != http.StatusAccepted {
			t.Fatalf("secret lost after a co-editor saved: %d %s", r.Code, r.Body)
		}
	})
	t.Run("a new value replaces the secret", func(t *testing.T) {
		rec := owner.send("PUT", "/api/workflows/"+id, update(stored(), map[string]any{"graph": hookGraph(second, "token")}))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		assertNoLeak(t, second, map[string]string{"update response": rec.Body.String(), "GET": owner.send("GET", "/api/workflows/"+id, nil).Body.String()})
		if r := callHook(s, id, first); r.Code != http.StatusUnauthorized {
			t.Fatalf("old secret still accepted: %d", r.Code)
		}
		if r := callHook(s, id, second); r.Code != http.StatusAccepted {
			t.Fatalf("new secret not accepted: %d %s", r.Code, r.Body)
		}
	})
	t.Run("null cannot clear a secret that authentication needs", func(t *testing.T) {
		code, body := owner.do("PUT", "/api/workflows/"+id, update(stored(), map[string]any{"graph": hookGraph(nil, "token")}))
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "secret is required") {
			t.Fatalf("%d %v", code, body)
		}
		if r := callHook(s, id, second); r.Code != http.StatusAccepted {
			t.Fatalf("refused update changed the secret: %d", r.Code)
		}
	})
	t.Run("null clears the secret", func(t *testing.T) {
		got := obj(t, owner.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(stored(), map[string]any{"graph": hookGraph(nil, "none")})))
		if _, ok := hookParams(t, got)["secret"]; ok {
			t.Fatalf("secret still marked after clearing: %v", hookParams(t, got))
		}
		if _, ok := hookParams(t, stored())["secret"]; ok {
			t.Fatal("secret still stored")
		}
		if r := callHook(s, id, ""); r.Code != http.StatusAccepted {
			t.Fatalf("unauthenticated webhook: %d %s", r.Code, r.Body)
		}
	})
}

func TestPluginConfigSecrets(t *testing.T) {
	const secret = "gmail-client-SECRET-acme"
	s := newTestServer(t)
	admin := s.as(orgAdmin)
	path := "/api/plugins/gmail/config"
	setting := func(t *testing.T, v any, name string) map[string]any {
		t.Helper()
		for _, e := range list(t, v) {
			if obj(t, e)["name"] == name {
				return obj(t, e)
			}
		}
		t.Fatalf("no setting %s in %v", name, v)
		return nil
	}

	rec := admin.send("PUT", path, map[string]any{"values": map[string]any{"clientId": "cid-acme", "clientSecret": secret}})
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody(t, rec)
	if id := setting(t, got, "clientId"); id["set"] != true || id["value"] != "cid-acme" || id["kind"] != "config" {
		t.Fatalf("clientId: %v", id)
	}
	if sec := setting(t, got, "clientSecret"); sec["set"] != true || sec["value"] != nil {
		t.Fatalf("clientSecret: %v", sec)
	}
	assertNoLeak(t, secret, map[string]string{
		"PUT response": rec.Body.String(),
		"GET":          admin.send("GET", path, nil).Body.String(),
		"plugins list": admin.send("GET", "/api/plugins", nil).Body.String(),
		"nodes":        admin.send("GET", "/api/nodes", nil).Body.String(),
	})

	t.Run("only organization admins", func(t *testing.T) {
		for _, u := range []testUser{alice, pluginAdmin} {
			for _, m := range []string{"GET", "PUT"} {
				code, body := s.as(u).do(m, path, map[string]any{"values": map[string]any{"clientId": "hijack"}})
				if code != http.StatusForbidden || !strings.Contains(errorOf(t, body), "organization admin") {
					t.Errorf("%s %s: %d %v", u.Sub, m, code, body)
				}
			}
		}
		if v := setting(t, admin.must(http.StatusOK, "GET", path, nil), "clientId"); v["value"] != "cid-acme" {
			t.Fatalf("refused write changed config: %v", v)
		}
	})
	t.Run("config is per organization", func(t *testing.T) {
		other := s.as(otherAdmin)
		if v := setting(t, other.must(http.StatusOK, "GET", path, nil), "clientId"); v["set"] != false {
			t.Fatalf("other org sees acme's config: %v", v)
		}
		other.must(http.StatusOK, "PUT", path, map[string]any{"values": map[string]any{"clientId": "cid-other", "clientSecret": ""}})
		if v := setting(t, admin.must(http.StatusOK, "GET", path, nil), "clientId"); v["value"] != "cid-acme" {
			t.Fatalf("other org changed acme's config: %v", v)
		}
		if v := setting(t, admin.must(http.StatusOK, "GET", path, nil), "clientSecret"); v["set"] != true {
			t.Fatalf("other org cleared acme's secret: %v", v)
		}
		// The stored values are really used, per organization: acme can start
		// an OAuth flow with its client id, the other org has no secret.
		u := obj(t, s.as(alice).must(http.StatusOK, "POST", "/api/connections/authorize", map[string]any{"module": "gmail"}))
		if !strings.Contains(u["url"].(string), "client_id=cid-acme") {
			t.Fatalf("authorize URL %v", u["url"])
		}
		code, body := s.as(carol).do("POST", "/api/connections/authorize", map[string]any{"module": "gmail"})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "admin must first set") {
			t.Fatalf("other org: %d %v", code, body)
		}
	})
	t.Run("the reference keeps a secret, empty deletes it", func(t *testing.T) {
		got := admin.must(http.StatusOK, "PUT", path, map[string]any{"values": map[string]any{"clientSecret": secretRef, "clientId": "cid-2"}})
		if v := setting(t, got, "clientSecret"); v["set"] != true {
			t.Fatalf("reference dropped the secret: %v", v)
		}
		got = admin.must(http.StatusOK, "PUT", path, map[string]any{"values": map[string]any{"clientSecret": ""}})
		if v := setting(t, got, "clientSecret"); v["set"] != false {
			t.Fatalf("empty value kept the secret: %v", v)
		}
		if v := setting(t, got, "clientId"); v["value"] != "cid-2" {
			t.Fatalf("omitted key changed: %v", v)
		}
	})
	t.Run("unknown key and module", func(t *testing.T) {
		code, body := admin.do("PUT", path, map[string]any{"values": map[string]any{"nope": "x"}})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), `unknown config key "nope"`) {
			t.Fatalf("%d %v", code, body)
		}
		for _, m := range []string{"GET", "PUT"} {
			if code, body := admin.do(m, "/api/plugins/nope/config", map[string]any{"values": map[string]any{}}); code != http.StatusNotFound {
				t.Fatalf("%s unknown module: %d %v", m, code, body)
			}
		}
	})
}

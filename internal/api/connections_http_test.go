package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newCredential creates a GitHub credential as c.
func (c *client) newCredential(name, token string) map[string]any {
	c.s.t.Helper()
	return obj(c.s.t, c.must(http.StatusCreated, "POST", "/api/connections",
		map[string]any{"module": "github", "name": name, "fields": map[string]any{"token": token}}))
}

// visible returns the connection ids u can list.
func visible(t *testing.T, s *testServer, u testUser) []string {
	t.Helper()
	return ids(t, s.as(u).must(http.StatusOK, "GET", "/api/connections", nil))
}

func TestCredentialSecretsNeverReturned(t *testing.T) {
	const token, token2 = "ghp_FIRSTsecretTOKEN", "ghp_SECONDsecretTOKEN"
	s := newTestServer(t)
	a := s.as(alice)

	rec := a.send("POST", "/api/connections", map[string]any{"module": "github", "name": " My GitHub ", "fields": map[string]any{"token": token}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	c := obj(t, decodeBody(t, rec))
	id := c["id"].(string)
	if c["kind"] != "credential" || c["name"] != "My GitHub" || c["scope"] != "private" || c["ownerSub"] != "alice" ||
		c["orgId"] != "acme" || c["canManage"] != true || mustJSON(t, c["secrets"]) != `{"token":true}` {
		t.Fatalf("created: %v", c)
	}
	if f, _ := c["fields"].(map[string]any); f["token"] != nil {
		t.Fatalf("secret field among plain fields: %v", c["fields"])
	}

	bodies := map[string]string{"create": rec.Body.String(), "list": a.send("GET", "/api/connections", nil).Body.String()}
	upd := a.send("PATCH", "/api/connections/"+id, map[string]any{"fields": map[string]any{"token": token2}})
	if upd.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", upd.Code, upd.Body)
	}
	bodies["patch"] = upd.Body.String()
	bodies["list after patch"] = a.send("GET", "/api/connections", nil).Body.String()
	assertNoLeak(t, token, bodies)
	assertNoLeak(t, token2, bodies)

	t.Run("reference keeps, non-secret fields are returned, empty clears", func(t *testing.T) {
		got := obj(t, a.must(http.StatusOK, "PATCH", "/api/connections/"+id, map[string]any{
			"fields": map[string]any{"token": secretRef, "apiUrl": "https://api.acme.ghe.com"}}))
		if mustJSON(t, got["secrets"]) != `{"token":true}` || obj(t, got["fields"])["apiUrl"] != "https://api.acme.ghe.com" {
			t.Fatalf("%v", got)
		}
		code, body := a.do("PATCH", "/api/connections/"+id, map[string]any{"fields": map[string]any{"token": ""}})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "is required") {
			t.Fatalf("clearing a required secret: %d %v", code, body)
		}
	})
}

func TestCredentialValidation(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown module", map[string]any{"module": "nope", "fields": map[string]any{}}, `plugin "nope" does not use credentials`},
		{"OAuth-only module", map[string]any{"module": "gmail", "fields": map[string]any{}}, "does not use credentials"},
		{"unknown field", map[string]any{"module": "github", "fields": map[string]any{"token": "t", "password": "x"}}, `unknown field "password"`},
		{"required field missing", map[string]any{"module": "github", "fields": map[string]any{}}, "is required"},
		{"non-string value", map[string]any{"module": "github", "fields": map[string]any{"token": 5}}, "must be a string"},
		{"URL on a host the module does not allow", map[string]any{"module": "github", "fields": map[string]any{
			"token": "t", "apiUrl": "https://evil.example.com"}}, "evil.example.com"},
		{"secret URL on a disallowed host", map[string]any{"module": "slack", "fields": map[string]any{
			"webhookUrl": "https://evil.example.com/services/x"}}, "evil.example.com"},
		{"plain http URL", map[string]any{"module": "slack", "fields": map[string]any{
			"webhookUrl": "http://hooks.slack.com/services/x"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := a.do("POST", "/api/connections", c.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d: %v", code, body)
			}
			if msg := errorOf(t, body); !strings.Contains(msg, c.want) {
				t.Fatalf("error %q does not mention %q", msg, c.want)
			}
		})
	}
	rec := a.raw("POST", "/api/connections", strings.NewReader("{"), http.Header{"Content-Type": {"application/json"}})
	if rec.Code != http.StatusBadRequest || errorOf(t, decodeBody(t, rec)) != "invalid JSON" {
		t.Fatalf("invalid JSON: %d %s", rec.Code, rec.Body)
	}
	if n := len(visible(t, s, alice)); n != 0 {
		t.Fatalf("rejected credentials were stored: %d", n)
	}
	a.must(http.StatusCreated, "POST", "/api/connections", map[string]any{"module": "slack", "fields": map[string]any{
		"webhookUrl": "https://hooks.slack.com/services/T/B/x"}})
}

func TestConnectionSharing(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	id := a.newCredential("shared", "ghp_x")["id"].(string)
	path := "/api/connections/" + id

	cast := map[string]testUser{}
	for _, u := range []testUser{alice, bob, dave, erin, carol, orgAdmin, otherAdmin} {
		cast[u.Sub] = u
	}
	check := func(t *testing.T, sees map[string]bool) {
		t.Helper()
		for sub, want := range sees {
			u := cast[sub]
			if got := contains(visible(t, s, u), id); got != want {
				t.Errorf("%s sees the connection: %v, want %v", u.Sub, got, want)
			}
		}
	}
	t.Run("private: owner and org admin only", func(t *testing.T) {
		check(t, map[string]bool{"alice": true, "admin": true, "bob": false, "dave": false, "carol": false, "admin2": false})
		for _, e := range list(t, s.as(orgAdmin).must(http.StatusOK, "GET", "/api/connections", nil)) {
			if obj(t, e)["canManage"] != true {
				t.Errorf("org admin cannot manage: %v", e)
			}
		}
	})
	t.Run("group: members of the group (and subgroups)", func(t *testing.T) {
		a.must(http.StatusOK, "PATCH", path, map[string]any{"scope": "group", "groups": []any{"eng", " "}})
		got := obj(t, a.must(http.StatusOK, "GET", "/api/connections?module=github", nil).([]any)[0])
		if mustJSON(t, got["groups"]) != `["/eng"]` {
			t.Errorf("groups not normalized: %v", got["groups"])
		}
		check(t, map[string]bool{"dave": true, "erin": true, "bob": false, "carol": false})
		for _, e := range list(t, s.as(dave).must(http.StatusOK, "GET", "/api/connections", nil)) {
			if obj(t, e)["canManage"] != false {
				t.Errorf("group member can manage: %v", e)
			}
		}
	})
	t.Run("org: everyone in the org, nobody outside", func(t *testing.T) {
		a.must(http.StatusOK, "PATCH", path, map[string]any{"scope": "org"})
		check(t, map[string]bool{"bob": true, "dave": true, "carol": false, "admin2": false})
	})
	t.Run("module filter", func(t *testing.T) {
		if n := len(list(t, a.must(http.StatusOK, "GET", "/api/connections?module=slack", nil))); n != 0 {
			t.Fatalf("filter returned %d", n)
		}
	})
	t.Run("only owner or org admin may rename, share or delete", func(t *testing.T) {
		// bob can use it (org scope) but not manage it; carol cannot see it.
		for _, c := range []struct {
			u    testUser
			want int
		}{{bob, http.StatusForbidden}, {carol, http.StatusNotFound}, {otherAdmin, http.StatusNotFound}} {
			for _, m := range []string{"PATCH", "DELETE"} {
				code, body := s.as(c.u).do(m, path, map[string]any{"name": "hijacked", "scope": "private"})
				if code != c.want {
					t.Errorf("%s %s: %d, want %d (%v)", c.u.Sub, m, code, c.want, body)
				}
			}
		}
		got := obj(t, a.must(http.StatusOK, "GET", "/api/connections", nil).([]any)[0])
		if got["name"] != "shared" || got["scope"] != "org" {
			t.Fatalf("refused writes changed the connection: %v", got)
		}
		a.must(http.StatusOK, "PATCH", path, map[string]any{"scope": "private"})
		s.as(bob).must(http.StatusNotFound, "PATCH", path, map[string]any{"name": "x"})
		got = obj(t, s.as(orgAdmin).must(http.StatusOK, "PATCH", path, map[string]any{"name": " by admin "}))
		if got["name"] != "by admin" || got["ownerSub"] != "alice" {
			t.Fatalf("admin rename: %v", got)
		}
		s.as(bob).must(http.StatusNotFound, "DELETE", path, nil)
		s.as(orgAdmin).must(http.StatusNoContent, "DELETE", path, nil)
		a.must(http.StatusNotFound, "DELETE", path, nil)
		a.must(http.StatusNotFound, "PATCH", "/api/connections/not-a-uuid", map[string]any{"name": "x"})
	})
}

// A PATCH that is refused must not have changed anything. Regression test:
// updateConnection used to save the fields before it validated the scope.
func TestRefusedConnectionUpdateChangesNothing(t *testing.T) {
	s := newTestServer(t)
	a := s.as(alice)
	id := a.newCredential("atomic", "ghp_x")["id"].(string)
	code, body := a.do("PATCH", "/api/connections/"+id, map[string]any{
		"fields": map[string]any{"apiUrl": "https://api.acme.ghe.com"}, "scope": "public"})
	if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "scope must be") {
		t.Fatalf("%d %v", code, body)
	}
	got := obj(t, a.must(http.StatusOK, "GET", "/api/connections", nil).([]any)[0])
	if f, _ := got["fields"].(map[string]any); f["apiUrl"] != nil {
		t.Fatalf("BUG: the refused PATCH stored its fields anyway: %v", got["fields"])
	}
}

func TestWorkflowConnectionRefs(t *testing.T) {
	s := newTestServer(t)
	a, b := s.as(alice), s.as(bob)
	credA := a.newCredential("alice A", "ghp_a")["id"].(string)
	credB := a.newCredential("alice B", "ghp_b")["id"].(string)
	slack := obj(t, a.must(http.StatusCreated, "POST", "/api/connections", map[string]any{"module": "slack",
		"fields": map[string]any{"webhookUrl": "https://hooks.slack.com/services/T/B/x"}}))["id"].(string)
	gh := func(conn string) map[string]any {
		return graph([]map[string]any{
			node("start", "trigger.manual", nil),
			node("repo", "github.repo.get", map[string]any{"connection": conn, "owner": "o", "repo": "r"}),
		}, edge("start", "repo"))
	}
	refused := func(t *testing.T, c *client, method, path string, body any) {
		t.Helper()
		code, resp := c.do(method, path, body)
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, resp), "account not found or not shared with you") {
			t.Fatalf("%s %s: %d %v", method, path, code, resp)
		}
	}

	t.Run("selecting someone else's private credential is refused", func(t *testing.T) {
		refused(t, b, "POST", "/api/workflows", wfBody("bob", gh(credA), nil))
		refused(t, s.as(carol), "POST", "/api/workflows", wfBody("carol", gh(credA), nil))
	})
	t.Run("unknown id and credential of another module are refused", func(t *testing.T) {
		refused(t, a, "POST", "/api/workflows", wfBody("x", gh("00000000-0000-0000-0000-000000000000"), nil))
		refused(t, a, "POST", "/api/workflows", wfBody("x", gh(slack), nil))
	})
	t.Run("own credential is allowed", func(t *testing.T) {
		a.must(http.StatusCreated, "POST", "/api/workflows", wfBody("alice", gh(credA), nil))
	})
	t.Run("co-editor may keep an unchanged selection but not pick a new one", func(t *testing.T) {
		wf := a.createWF("co-edited", gh(credA), shared("/sales", "editor"))
		id := wf["id"].(string)
		wf = obj(t, b.must(http.StatusOK, "PUT", "/api/workflows/"+id, update(wf, map[string]any{"name": "renamed by bob"})))
		refused(t, b, "PUT", "/api/workflows/"+id, update(wf, map[string]any{"graph": gh(credB)}))
		stored := obj(t, a.must(http.StatusOK, "GET", "/api/workflows/"+id, nil))
		if !strings.Contains(mustJSON(t, stored["graph"]), credA) || stored["version"] != wf["version"] {
			t.Fatalf("refused save changed the workflow: %v", stored)
		}
	})
	t.Run("a credential shared with the user's group may be selected", func(t *testing.T) {
		a.must(http.StatusOK, "PATCH", "/api/connections/"+credB, map[string]any{"scope": "group", "groups": []any{"/sales"}})
		b.must(http.StatusCreated, "POST", "/api/workflows", wfBody("bob", gh(credB), nil))
	})
}

// fakeOAuthProvider is the token endpoint of a test OAuth2 module.
func fakeOAuthProvider(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/token" || r.PostForm.Get("code") != "good" || r.PostForm.Get("client_secret") != "app-secret" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "oauth-ACCESS-token-value", "refresh_token": "oauth-REFRESH-value",
			"token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOAuthCallback(t *testing.T) {
	s := newTestServer(t)
	provider := fakeOAuthProvider(t)
	// The provider side is a test module whose token endpoint is the fake
	// provider; the whole flow runs through the real router.
	s.importPlugin("fakeoauth.yaml", `module: fakeoauth
name: Fake OAuth
baseUrl: https://api.example.com
config:
  - { name: clientId, label: Client ID }
  - { name: clientSecret, label: Client secret, type: secret }
oauth2:
  authorizeUrl: `+provider.URL+`/authorize
  tokenUrl: `+provider.URL+`/token
  pkce: true
nodes:
  - { type: get, name: Get, request: { path: /x } }
`)
	s.as(orgAdmin).must(http.StatusOK, "PUT", "/api/plugins/fakeoauth/config",
		map[string]any{"values": map[string]any{"clientId": "app-id", "clientSecret": "app-secret"}})

	// start begins a flow as alice and returns the state and the cookie nonce.
	start := func(t *testing.T) (state, nonce string) {
		t.Helper()
		rec := s.as(alice).send("POST", "/api/connections/authorize", map[string]any{"module": "fakeoauth", "name": "Alice's account"})
		if rec.Code != http.StatusOK {
			t.Fatalf("authorize: %d %s", rec.Code, rec.Body)
		}
		u, err := url.Parse(obj(t, decodeBody(t, rec))["url"].(string))
		if err != nil || !strings.HasPrefix(u.String(), provider.URL+"/authorize?") {
			t.Fatalf("authorize URL %v", u)
		}
		if q := u.Query(); q.Get("client_id") != "app-id" || q.Get("redirect_uri") != testPublicURL+"/oauth/callback" || q.Get("code_challenge") == "" {
			t.Fatalf("authorize query %v", q)
		}
		var cookie *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == "g2f_oauth" {
				cookie = c
			}
		}
		if cookie == nil || !cookie.HttpOnly || cookie.Path != "/oauth/callback" || cookie.SameSite != http.SameSiteLaxMode || cookie.Value == "" {
			t.Fatalf("flow cookie %+v", cookie)
		}
		return u.Query().Get("state"), cookie.Value
	}
	oauthConns := func() int {
		n := 0
		for _, e := range list(t, s.as(orgAdmin).must(http.StatusOK, "GET", "/api/connections?module=fakeoauth", nil)) {
			if obj(t, e)["kind"] == "oauth2" {
				n++
			}
		}
		return n
	}
	callback := func(query url.Values, cookie string) *httptest.ResponseRecorder {
		hdr := http.Header{}
		if cookie != "" {
			hdr.Set("Cookie", "g2f_oauth="+cookie)
		}
		return s.anon().raw("GET", "/oauth/callback?"+query.Encode(), nil, hdr)
	}

	state, nonce := start(t)
	failures := []struct {
		name   string
		query  url.Values
		cookie string
		want   string
	}{
		{"missing code", url.Values{"state": {state}}, nonce, "missing code or state"},
		{"missing state", url.Values{"code": {"good"}}, nonce, "missing code or state"},
		{"provider error", url.Values{"error": {"access_denied"}, "error_description": {"<script>alert(1)</script>"}}, nonce, "access_denied"},
		{"tampered state", url.Values{"code": {"good"}, "state": {state[:len(state)-4] + "AAAA"}}, nonce, "invalid or tampered state"},
		{"no flow cookie", url.Values{"code": {"good"}, "state": {state}}, "", "started in another browser"},
		{"cookie of another flow", url.Values{"code": {"good"}, "state": {state}}, "wrong-nonce", "started in another browser"},
		{"code refused by the provider", url.Values{"code": {"bad"}, "state": {state}}, nonce, "provider refused the token request"},
	}
	for _, c := range failures {
		t.Run("error page: "+c.name, func(t *testing.T) {
			rec := callback(c.query, c.cookie)
			body := rec.Body.String()
			if rec.Code != http.StatusBadRequest || !strings.Contains(body, "Connection failed") || !strings.Contains(body, c.want) {
				t.Fatalf("%d, want 400 page mentioning %q: %s", rec.Code, c.want, body)
			}
			if strings.Contains(body, "<script>alert(1)") {
				t.Fatal("provider-supplied text is not escaped")
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("headers %v", rec.Header())
			}
			if n := oauthConns(); n != 0 {
				t.Fatalf("a failed callback created %d connection(s)", n)
			}
		})
	}

	t.Run("a valid callback creates a private connection of the user who started it", func(t *testing.T) {
		state, nonce := start(t)
		rec := callback(url.Values{"code": {"good"}, "state": {state}}, nonce)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Account connected") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "g2f_oauth=;") || !strings.Contains(sc, "Max-Age=0") {
			t.Errorf("flow cookie not cleared: %q", sc)
		}
		listRec := s.as(alice).send("GET", "/api/connections?module=fakeoauth", nil)
		got := list(t, decodeBody(t, listRec))
		if len(got) != 1 {
			t.Fatalf("connections %v", got)
		}
		c := obj(t, got[0])
		if c["kind"] != "oauth2" || c["ownerSub"] != "alice" || c["orgId"] != "acme" || c["scope"] != "private" || c["name"] != "Alice's account" {
			t.Fatalf("connection %v", c)
		}
		assertNoLeak(t, "oauth-ACCESS-token-value", map[string]string{"list": listRec.Body.String(), "callback": rec.Body.String()})
		assertNoLeak(t, "oauth-REFRESH-value", map[string]string{"list": listRec.Body.String(), "callback": rec.Body.String()})
		if contains(visible(t, s, bob), c["id"].(string)) {
			t.Fatal("bob sees alice's new private account")
		}
	})
	t.Run("authorize needs an OAuth module with app credentials", func(t *testing.T) {
		code, body := s.as(alice).do("POST", "/api/connections/authorize", map[string]any{"module": "github"})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "does not support connected accounts") {
			t.Fatalf("%d %v", code, body)
		}
		code, body = s.as(carol).do("POST", "/api/connections/authorize", map[string]any{"module": "fakeoauth"})
		if code != http.StatusBadRequest || !strings.Contains(errorOf(t, body), "admin must first set") {
			t.Fatalf("other org without app credentials: %d %v", code, body)
		}
	})
}

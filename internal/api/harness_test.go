package api

// HTTP-level test harness: a real Server (router, middleware, handlers) on a
// per-test PostgreSQL database, the real node registry with the bundled
// modules, the real engine, secrets, credentials, OAuth and plugin services,
// and OIDC authentication against a fake identity provider that signs RS256
// access tokens. Tests that need the database are skipped when
// G2F_TEST_DATABASE_URL is not set (see internal/store/storetest).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/config"
	"github.com/glue2flow/g2f/internal/credentials"
	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/oauth"
	"github.com/glue2flow/g2f/internal/plugin"
	"github.com/glue2flow/g2f/internal/push"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/store/storetest"
	"github.com/glue2flow/g2f/internal/trigger"
	"github.com/glue2flow/g2f/modules/email"
)

const (
	testClientID    = "g2f"
	testPublicURL   = "http://g2f.test"
	adminRole       = "g2f-admin"
	pluginAdminRole = "g2f-plugin-admin"
)

// ---------------------------------------------------------------------------
// Fake identity provider (shared by all tests of the package)

type fakeIdP struct {
	srv    *httptest.Server
	issuer string
	signer jose.Signer
	// rogue signs with a key that is not in the JWKS but claims the same kid.
	rogue jose.Signer
}

var idp *fakeIdP

func TestMain(m *testing.M) {
	idp = newFakeIdP()
	code := m.Run()
	idp.srv.Close()
	os.Exit(code)
}

func newFakeIdP() *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	mk := func(k *rsa.PrivateKey) jose.Signer {
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k, KeyID: "k1"}},
			(&jose.SignerOptions{}).WithType("JWT"))
		if err != nil {
			panic(err)
		}
		return s
	}
	mux := http.NewServeMux()
	p := &fakeIdP{srv: httptest.NewServer(mux), signer: mk(key), rogue: mk(other)}
	p.issuer = p.srv.URL + "/realms/g2f"
	mux.HandleFunc("/realms/g2f/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	return p
}

// testUser describes the claims of a signed-in user.
type testUser struct {
	Sub, Name, Org string
	Groups, Roles  []string
}

// The cast. Group paths follow Keycloak's full-path convention.
var (
	// alice owns most test data.
	alice = testUser{Sub: "alice", Name: "Alice", Org: "acme", Groups: []string{"/eng/backend"}}
	// bob is in acme, but in another group than alice.
	bob = testUser{Sub: "bob", Name: "Bob", Org: "acme", Groups: []string{"/sales"}}
	// dave is in a subgroup of /eng, like alice.
	dave = testUser{Sub: "dave", Name: "Dave", Org: "acme", Groups: []string{"/eng/frontend"}}
	// carol is in another organization, with the same group names as alice.
	carol = testUser{Sub: "carol", Name: "Carol", Org: "other", Groups: []string{"/eng/backend", "/sales"}}
	// orgAdmin administers acme.
	orgAdmin = testUser{Sub: "admin", Name: "Admin", Org: "acme", Roles: []string{adminRole}}
	// otherAdmin administers the other organization.
	otherAdmin = testUser{Sub: "admin2", Name: "Other Admin", Org: "other", Roles: []string{adminRole}}
	// pluginAdmin may install plugins (installation-wide) but is no org admin.
	pluginAdmin = testUser{Sub: "padmin", Name: "Plugin Admin", Org: "acme", Roles: []string{pluginAdminRole}}
)

// claims returns the access token claims of u, as Keycloak would issue them.
func (u testUser) claims() map[string]any {
	now := time.Now()
	groups := u.Groups
	if groups == nil {
		groups = []string{}
	}
	roles := u.Roles
	if roles == nil {
		roles = []string{}
	}
	return map[string]any{
		"iss": idp.issuer, "sub": u.Sub, "azp": testClientID, "aud": "account",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(),
		"preferred_username": u.Sub, "name": u.Name, "email": u.Sub + "@" + u.Org + ".test",
		"org": u.Org, "groups": groups, "realm_access": map[string]any{"roles": roles},
	}
}

// sign returns a compact JWS of claims.
func (p *fakeIdP) sign(t testing.TB, s jose.Signer, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := s.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// token returns a valid access token of u.
func (u testUser) token(t testing.TB) string { return idp.sign(t, idp.signer, u.claims()) }

// ---------------------------------------------------------------------------
// Test server

// serverOptions vary the configuration of a test server.
type serverOptions struct {
	trustedProxies []netip.Prefix
	requireSigned  bool
	catalogs       []string
	// staticDir defaults to an empty temporary directory.
	staticDir string
}

type testServer struct {
	t       *testing.T
	handler http.Handler
	cfg     *config.Config
	st      *store.Store
	reg     *nodes.Registry
	eng     *engine.Engine
	plugins *plugin.Manager
	signer  *trigger.ResumeSigner
}

// newTestServer wires a Server like cmd/g2f does, on a fresh database.
func newTestServer(t *testing.T) *testServer {
	return newTestServerWith(t, serverOptions{})
}

func newTestServerWith(t *testing.T, o serverOptions) *testServer {
	t.Helper()
	st := storetest.DB(t) // skips without G2F_TEST_DATABASE_URL
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cfg := &config.Config{
		PublicURL: testPublicURL, StaticDir: o.staticDir, ModulesDir: "../../modules",
		SecretKey: base64.StdEncoding.EncodeToString(key),
		Auth: config.AuthConfig{
			Mode: config.AuthOIDC, Issuer: idp.issuer, ClientID: testClientID,
			GroupsClaim: "groups", OrgClaim: "org", AdminRole: adminRole, PluginAdminRole: pluginAdminRole,
			DefaultOrg: "default",
		},
		PluginRequireSigned: o.requireSigned, PluginCatalogs: o.catalogs, TrustedProxies: o.trustedProxies,
		Workers: 4, ExecutionTimeout: 30 * time.Second, MaxStoredItems: 100, WebhookMaxWait: 10 * time.Second,
	}
	if cfg.StaticDir == "" {
		cfg.StaticDir = t.TempDir()
	}

	box, err := secrets.NewBox(cfg.SecretKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	plugins := plugin.NewManager(reg, st, log)
	plugins.SetTrust(plugin.Trust{RequireSigned: cfg.PluginRequireSigned})
	if _, err := plugins.LoadDir(ctx, cfg.ModulesDir); err != nil {
		t.Fatalf("load modules: %v", err)
	}
	if err := plugins.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	sec := secrets.NewService(box, st, reg)
	authn, err := auth.New(context.Background(), cfg.Auth)
	if err != nil {
		t.Fatal(err)
	}
	oauthMgr := oauth.NewManager(st, box, reg, sec, cfg.PublicURL)
	creds := credentials.NewService(st, box, reg)
	signer := trigger.NewResumeSigner(cfg.SecretKey, cfg.PublicURL)
	eng := engine.New(reg, st, log, engine.Options{
		Secrets: sec, Tokens: oauthMgr, Credentials: creds, State: st, Workflows: st,
		ResumeURL: signer.URL, Workers: cfg.Workers, Timeout: cfg.ExecutionTimeout, MaxStoredItems: cfg.MaxStoredItems,
	})
	if err := eng.Register(ctx); err != nil {
		t.Fatal(err)
	}
	// The scheduler and the push service are change listeners and serve
	// /push/*; their background loops are not needed by these tests.
	sched := trigger.NewScheduler(st, eng, reg, log, trigger.SchedulerOptions{Instance: eng.InstanceID(), CheckWorkers: 1})
	pushSvc := push.New(push.Options{
		Store: st, Registry: reg, Box: box, Waker: sched.Checks(), Contexts: eng, Tokens: oauthMgr, Settings: sec, Log: log,
		Instance: eng.InstanceID(), PublicURL: cfg.PublicURL, Enabled: false,
	})
	srv := New(Deps{
		Config: cfg, Store: st, Registry: reg, Engine: eng, Auth: authn, Logger: log,
		Listener: Listeners{sched, pushSvc},
		Webhooks: trigger.NewWebhookHandler(st, eng, reg, sec, log, cfg.WebhookMaxWait),
		Forms: trigger.NewFormHandler(st, eng, reg, log, trigger.FormOptions{
			SecretKey: cfg.SecretKey, PublicURL: cfg.PublicURL,
			State: st, Auth: authn, Mailer: email.OrgMailer{Settings: sec.ModuleSettings},
		}),
		Resume:      trigger.NewResumeHandler(signer, eng, log),
		Secrets:     sec,
		Plugins:     plugins,
		Catalogs:    plugin.NewCatalogs(cfg.PluginCatalogs, plugins, "test"),
		OAuth:       oauthMgr,
		Credentials: creds,
		Push:        pushSvc.Handler(),
		Delivery:    pushSvc,
	})
	// Runs before the database is dropped (cleanups run in reverse order):
	// running executions finish, the instance is unregistered.
	t.Cleanup(func() { eng.Shutdown(5*time.Second, 5*time.Second) })
	return &testServer{t: t, handler: srv.Handler(), cfg: cfg, st: st, reg: reg, eng: eng, plugins: plugins, signer: signer}
}

// ---------------------------------------------------------------------------
// Clients

type client struct {
	s     *testServer
	token string
	// header is added to every request.
	header http.Header
}

// as returns a client that sends u's access token.
func (s *testServer) as(u testUser) *client {
	return &client{s: s, token: u.token(s.t), header: http.Header{}}
}

// anon returns a client without a token.
func (s *testServer) anon() *client { return &client{s: s, header: http.Header{}} }

// withToken returns a client that sends an arbitrary bearer token.
func (s *testServer) withToken(tok string) *client {
	return &client{s: s, token: tok, header: http.Header{}}
}

// raw sends a request with the given body and returns the recorded response.
func (c *client) raw(method, path string, body io.Reader, hdr http.Header) *httptest.ResponseRecorder {
	c.s.t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range c.header {
		req.Header[k] = v
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	rec := httptest.NewRecorder()
	c.s.handler.ServeHTTP(rec, req)
	return rec
}

// send marshals body (if not nil) as JSON and returns the recorded response.
func (c *client) send(method, path string, body any) *httptest.ResponseRecorder {
	c.s.t.Helper()
	var r io.Reader
	hdr := http.Header{}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.s.t.Fatal(err)
		}
		r = bytes.NewReader(b)
		hdr.Set("Content-Type", "application/json")
	}
	return c.raw(method, path, r, hdr)
}

// do sends a JSON request and returns the status and the decoded JSON body
// (nil when the body is empty or not JSON).
func (c *client) do(method, path string, body any) (int, any) {
	c.s.t.Helper()
	rec := c.send(method, path, body)
	var out any
	if b := bytes.TrimSpace(rec.Body.Bytes()); len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			out = nil
		}
	}
	return rec.Code, out
}

// must sends a JSON request, checks the status and returns the decoded body.
func (c *client) must(want int, method, path string, body any) any {
	c.s.t.Helper()
	rec := c.send(method, path, body)
	if rec.Code != want {
		c.s.t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	var out any
	if b := bytes.TrimSpace(rec.Body.Bytes()); len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			c.s.t.Fatalf("%s %s: body is not JSON: %s", method, path, b)
		}
	}
	return out
}

// obj and list convert decoded JSON.
func obj(t testing.TB, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON object, got %T: %v", v, v)
	}
	return m
}

func list(t testing.TB, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a JSON array, got %T: %v", v, v)
	}
	return l
}

// errorOf returns the "error" field of an API error response.
func errorOf(t testing.TB, v any) string {
	t.Helper()
	m := obj(t, v)
	s, ok := m["error"].(string)
	if !ok || s == "" {
		t.Fatalf("expected {\"error\": \"...\"}, got %v", v)
	}
	return s
}

// ids returns the "id" fields of a list of objects.
func ids(t testing.TB, v any) []string {
	t.Helper()
	var out []string
	for _, e := range list(t, v) {
		out = append(out, obj(t, e)["id"].(string))
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Workflow builders

// node builds a graph node.
func node(id, typ string, params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	return map[string]any{"id": id, "type": typ, "name": id, "params": params, "position": map[string]any{"x": 0, "y": 0}}
}

func edge(src, dst string) map[string]any {
	return map[string]any{"id": src + "-" + dst, "source": src, "target": dst}
}

func graph(nodes []map[string]any, edges ...map[string]any) map[string]any {
	if edges == nil {
		edges = []map[string]any{}
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}

// manualGraph is the minimal valid workflow: manual trigger -> Set.
func manualGraph() map[string]any {
	return graph([]map[string]any{
		node("start", "trigger.manual", nil),
		node("set", "core.set", map[string]any{"fields": map[string]any{"hello": "world"}}),
	}, edge("start", "set"))
}

// waitGraph pauses at a Wait node until its resume URL is called.
func waitGraph() map[string]any {
	return graph([]map[string]any{
		node("start", "trigger.manual", nil),
		node("wait", "core.wait", map[string]any{"mode": nodes.WaitWebhook, "timeoutHours": 0}),
		node("log", "core.log", map[string]any{"message": "resumed"}),
	}, edge("start", "wait"), edge("wait", "log"))
}

// wfBody is a create/update request body.
func wfBody(name string, g map[string]any, extra map[string]any) map[string]any {
	b := map[string]any{"name": name, "graph": g, "scope": "private", "shares": []any{}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// createWF creates a workflow as c and returns it.
func (c *client) createWF(name string, g map[string]any, extra map[string]any) map[string]any {
	c.s.t.Helper()
	return obj(c.s.t, c.must(http.StatusCreated, "POST", "/api/workflows", wfBody(name, g, extra)))
}

// update builds a PUT body from a stored workflow with changes applied.
func update(wf map[string]any, changes map[string]any) map[string]any {
	b := map[string]any{}
	for _, k := range []string{"name", "description", "scope", "orgRole", "shares", "graph", "active", "version"} {
		b[k] = wf[k]
	}
	for k, v := range changes {
		b[k] = v
	}
	return b
}

// shared returns create options for a group-scoped workflow.
func shared(group, role string) map[string]any {
	return map[string]any{"scope": "group", "shares": []any{map[string]any{"group": group, "role": role}}}
}

// ---------------------------------------------------------------------------
// Executions

// waitExec polls an execution until done reports true or the deadline passes.
func (c *client) waitExec(id string, done func(status string) bool) map[string]any {
	c.s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ex := obj(c.s.t, c.must(http.StatusOK, "GET", "/api/executions/"+id, nil))
		if done(ex["status"].(string)) {
			return ex
		}
		if time.Now().After(deadline) {
			c.s.t.Fatalf("execution %s still %v", id, ex["status"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// finished: the execution ended. waiting: it paused (or already ended, so
// that a broken Wait node fails the test's assertion instead of timing out).
func finished(s string) bool {
	return s == string(store.StatusSuccess) || s == string(store.StatusError)
}

func waiting(s string) bool { return s == string(store.StatusWaiting) || finished(s) }

// eventually polls cond until it returns true or the deadline passes.
func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stepOutput returns the "main" output items of the step of node id.
func stepOutput(t testing.TB, exec map[string]any, id string) []any {
	t.Helper()
	for _, s := range list(t, exec["steps"]) {
		st := obj(t, s)
		if st["nodeId"] == id {
			out, _ := st["output"].(map[string]any)
			l, _ := out["main"].([]any)
			return l
		}
	}
	t.Fatalf("no step for node %s in %v", id, exec["steps"])
	return nil
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeFile creates dir/name with content.
func writeFile(t testing.TB, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertNoLeak fails if secret occurs in any of the responses.
func assertNoLeak(t testing.TB, secret string, bodies map[string]string) {
	t.Helper()
	for what, b := range bodies {
		if strings.Contains(b, secret) {
			t.Errorf("BUG: secret value leaked in %s: %s", what, b)
		}
	}
}

// upload sends a multipart form with a "file" part (if filename is set) and
// further fields, like the plugin import of the editor.
func (c *client) upload(path, filename, content string, fields map[string]string) *httptest.ResponseRecorder {
	c.s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			c.s.t.Fatal(err)
		}
		_, _ = io.WriteString(fw, content)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		c.s.t.Fatal(err)
	}
	return c.raw("POST", path, &buf, http.Header{"Content-Type": {mw.FormDataContentType()}})
}

// importPlugin imports a manifest as the plugin admin and fails on errors.
func (s *testServer) importPlugin(filename, manifest string) map[string]any {
	s.t.Helper()
	rec := s.as(pluginAdmin).upload("/api/plugins", filename, manifest, nil)
	if rec.Code != http.StatusCreated {
		s.t.Fatalf("import %s: %d %s", filename, rec.Code, rec.Body)
	}
	return obj(s.t, decodeBody(s.t, rec))
}

// decodeBody decodes a recorded JSON response body.
func decodeBody(t testing.TB, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON (%v): %q", err, rec.Body)
	}
	return out
}

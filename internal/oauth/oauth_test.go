package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
)

type memStore struct {
	mu    sync.Mutex
	conns map[string]*store.Connection
}

func (m *memStore) CreateConnection(_ context.Context, c *store.Connection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *c
	m.conns[c.ID] = &cp
	return nil
}

func (m *memStore) GetConnection(_ context.Context, id string) (*store.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *memStore) RefreshConnection(_ context.Context, id string, fn func(*store.Connection) (bool, error)) (*store.Connection, error) {
	m.mu.Lock() // plays the role of SELECT ... FOR UPDATE
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *c
	changed, err := fn(&cp)
	if err != nil {
		return nil, err
	}
	if changed {
		m.conns[id] = &cp
	}
	return &cp, nil
}

type settings map[string]any

func (s settings) ModuleSettings(context.Context, string, string) (map[string]any, map[string]any, error) {
	return s, map[string]any{}, nil
}

// fakeProvider is a minimal OAuth2 server with PKCE, refresh token rotation
// and a userinfo endpoint.
type fakeProvider struct {
	srv       *httptest.Server
	challenge string
	refreshes atomic.Int32
	expiresIn int
	// instanceURL, if set, is returned as instance_url (also on refresh unless
	// noInstanceOnRefresh).
	instanceURL         string
	noInstanceOnRefresh bool
}

// withInstance adds instance_url to a token response.
func (p *fakeProvider) withInstance(m map[string]any, refresh bool) map[string]any {
	if p.instanceURL != "" && !(refresh && p.noInstanceOnRefresh) {
		m["instance_url"] = p.instanceURL
	}
	return m
}

func newProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{expiresIn: 3600}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "app-id" || r.Form.Get("client_secret") != "app-secret" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
				return
			}
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad code or verifier"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(p.withInstance(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1",
					"token_type": "bearer", "expires_in": p.expiresIn, "scope": "sheets"}, false))
			case "refresh_token":
				n := p.refreshes.Add(1)
				want := "refresh-" + string(rune('0'+n))
				if r.Form.Get("refresh_token") != want {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"stale refresh token"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(p.withInstance(map[string]any{"access_token": "access-" + string(rune('1'+n)),
					"refresh_token": "refresh-" + string(rune('1'+n)), "expires_in": 3600}, true))
			}
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer access-1" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"email":"bob@acme.example"}`))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func setup(t *testing.T, p *fakeProvider) (*Manager, *memStore) {
	t.Helper()
	return setupWith(t, p, "")
}

// setupWith adds extra lines (indented by two spaces) to the oauth2 block.
func setupWith(t *testing.T, p *fakeProvider, oauth2Extra string) (*Manager, *memStore) {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := nodes.ParseManifest([]byte(`
module: sheets
name: Sheets
baseUrl: ` + p.srv.URL + `/api
config:
  - { name: clientId }
  - { name: clientSecret, type: secret }
oauth2:
  authorizeUrl: ` + p.srv.URL + `/authorize
  tokenUrl: ` + p.srv.URL + `/token
  scopes: [sheets, email]
  pkce: true
  authorizeParams: { access_type: offline }
  userInfo: { url: ` + p.srv.URL + `/userinfo, field: email }
` + oauth2Extra + `
nodes:
  - { type: read, name: Read, request: { path: /read } }
`))
	if err != nil {
		t.Fatal(err)
	}
	reg := nodes.NewRegistry()
	list, err := nodes.HTTPModuleNodes(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterModule(m.Info("test"), list); err != nil {
		t.Fatal(err)
	}
	st := &memStore{conns: map[string]*store.Connection{}}
	mgr := NewManager(st, box, reg, settings{"clientId": "app-id", "clientSecret": "app-secret"}, "https://g2f.test")
	return mgr, st
}

func connect(t *testing.T, mgr *Manager, p *fakeProvider) *store.Connection {
	t.Helper()
	u := &auth.User{Sub: "bob", Name: "Bob", Org: "acme"}
	raw, nonce, err := mgr.AuthorizeURL(context.Background(), u, "sheets", "My sheets")
	if err != nil {
		t.Fatal(err)
	}
	au, _ := url.Parse(raw)
	q := au.Query()
	if q.Get("client_id") != "app-id" || q.Get("redirect_uri") != "https://g2f.test/oauth/callback" ||
		q.Get("scope") != "sheets email" || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL %s", raw)
	}
	p.challenge = q.Get("code_challenge")
	c, err := mgr.Callback(context.Background(), "good-code", q.Get("state"), nonce)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAuthorizeCallbackAndRefresh(t *testing.T) {
	p := newProvider(t)
	mgr, st := setup(t, p)
	c := connect(t, mgr, p)
	if c.Account != "bob@acme.example" || c.OwnerSub != "bob" || c.Scope != "private" || c.Scopes != "sheets" {
		t.Fatalf("connection %+v", c)
	}
	if strings.Contains(c.AccessToken, "access-1") || strings.Contains(c.RefreshToken, "refresh-1") {
		t.Fatal("tokens must be stored encrypted")
	}

	tok, err := mgr.Token(context.Background(), "acme", "sheets", c.ID)
	if err != nil || tok.AccessToken != "access-1" || tok.AuthorizationHeader() != "Bearer access-1" {
		t.Fatalf("token %+v %v", tok, err)
	}
	if p.refreshes.Load() != 0 {
		t.Fatal("fresh token must not be refreshed")
	}

	// Wrong org or module: not found.
	if _, err := mgr.Token(context.Background(), "globex", "sheets", c.ID); err != ErrNotFound {
		t.Fatalf("cross-org token: %v", err)
	}

	// Expire it; concurrent users trigger exactly one refresh (rotation safe).
	st.mu.Lock()
	past := time.Now().Add(-time.Minute)
	st.conns[c.ID].ExpiresAt = &past
	st.mu.Unlock()
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := mgr.Token(context.Background(), "acme", "sheets", c.ID)
			if err != nil || tok.AccessToken != "access-2" {
				t.Errorf("after refresh: %+v %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if n := p.refreshes.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

func TestCallbackRejects(t *testing.T) {
	p := newProvider(t)
	mgr, _ := setup(t, p)
	if _, err := mgr.Callback(context.Background(), "good-code", "garbage", "n"); err == nil {
		t.Fatal("tampered state accepted")
	}
	u := &auth.User{Sub: "bob", Org: "acme"}
	raw, nonce, _ := mgr.AuthorizeURL(context.Background(), u, "sheets", "")
	au, _ := url.Parse(raw)
	p.challenge = au.Query().Get("code_challenge")
	if _, err := mgr.Callback(context.Background(), "bad-code", au.Query().Get("state"), nonce); err == nil ||
		!strings.Contains(err.Error(), "bad code") {
		t.Fatalf("bad code: %v", err)
	}
	// Expired state.
	mgr.now = func() time.Time { return time.Now().Add(stateTTL + time.Minute) }
	if _, err := mgr.Callback(context.Background(), "good-code", au.Query().Get("state"), nonce); err == nil {
		t.Fatal("expired state accepted")
	}
}

func TestAuthorizeNeedsAppCredentials(t *testing.T) {
	p := newProvider(t)
	mgr, _ := setup(t, p)
	mgr.settings = settings{}
	_, _, err := mgr.AuthorizeURL(context.Background(), &auth.User{Org: "acme"}, "sheets", "")
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("expected admin hint, got %v", err)
	}
}

func TestCallbackBindsBrowser(t *testing.T) {
	p := newProvider(t)
	mgr, st := setup(t, p)
	u := &auth.User{Sub: "bob", Org: "acme"}
	raw, nonce, err := mgr.AuthorizeURL(context.Background(), u, "sheets", "")
	if err != nil || len(nonce) < 16 {
		t.Fatalf("nonce %q %v", nonce, err)
	}
	au, _ := url.Parse(raw)
	state := au.Query().Get("state")
	p.challenge = au.Query().Get("code_challenge")

	for _, bad := range []string{"", "wrong-nonce"} {
		_, err := mgr.Callback(context.Background(), "good-code", state, bad)
		if err == nil || !strings.Contains(err.Error(), "another browser") {
			t.Fatalf("nonce %q: %v", bad, err)
		}
	}
	st.mu.Lock()
	n := len(st.conns)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d connections created by rejected callbacks", n)
	}
	if _, err := mgr.Callback(context.Background(), "good-code", state, nonce); err != nil {
		t.Fatalf("right nonce: %v", err)
	}
}

const instanceSpec = `  instanceUrlField: instance_url
  apiHosts: ["*.my.salesforce.com"]
`

func TestInstanceURLStoredAndKeptOnRefresh(t *testing.T) {
	p := newProvider(t)
	p.instanceURL = "https://acme.my.salesforce.com/"
	mgr, st := setupWith(t, p, instanceSpec)
	c := connect(t, mgr, p)
	if c.InstanceURL != "https://acme.my.salesforce.com" {
		t.Fatalf("stored %q", c.InstanceURL)
	}
	tok, err := mgr.Token(context.Background(), "acme", "sheets", c.ID)
	if err != nil || tok.InstanceURL != "https://acme.my.salesforce.com" {
		t.Fatalf("token %+v %v", tok, err)
	}

	expire := func() {
		st.mu.Lock()
		past := time.Now().Add(-time.Minute)
		st.conns[c.ID].ExpiresAt = &past
		st.mu.Unlock()
	}
	// A refresh response that repeats instance_url updates it ...
	p.instanceURL = "https://acme2.my.salesforce.com"
	expire()
	if tok, err = mgr.Token(context.Background(), "acme", "sheets", c.ID); err != nil || tok.AccessToken != "access-2" || tok.InstanceURL != "https://acme2.my.salesforce.com" {
		t.Fatalf("refresh with instance_url: %+v %v", tok, err)
	}
	// ... one that omits it keeps the stored value.
	p.noInstanceOnRefresh = true
	expire()
	if tok, err = mgr.Token(context.Background(), "acme", "sheets", c.ID); err != nil || tok.AccessToken != "access-3" || tok.InstanceURL != "https://acme2.my.salesforce.com" {
		t.Fatalf("refresh without instance_url: %+v %v", tok, err)
	}
}

func TestInstanceURLRejected(t *testing.T) {
	for _, bad := range []string{
		"http://acme.my.salesforce.com",
		"https://acme.my.salesforce.com/services",
		"https://user:pw@acme.my.salesforce.com",
		"https://acme.my.salesforce.com/?x=1",
		"https://acme.my.salesforce.com#frag",
		"https://evil.example.com",
		"https://my.salesforce.com",
		"//acme.my.salesforce.com",
		"acme.my.salesforce.com",
		"https://",
	} {
		p := newProvider(t)
		p.instanceURL = bad
		mgr, st := setupWith(t, p, instanceSpec)
		u := &auth.User{Sub: "bob", Org: "acme"}
		raw, nonce, err := mgr.AuthorizeURL(context.Background(), u, "sheets", "")
		if err != nil {
			t.Fatal(err)
		}
		au, _ := url.Parse(raw)
		p.challenge = au.Query().Get("code_challenge")
		_, err = mgr.Callback(context.Background(), "good-code", au.Query().Get("state"), nonce)
		if err == nil || !strings.Contains(err.Error(), "instance_url") {
			t.Errorf("%q: %v", bad, err)
		}
		if len(st.conns) != 0 {
			t.Errorf("%q: connection stored", bad)
		}
	}

	// A provider that names the field in the manifest but omits it: no connection.
	p := newProvider(t)
	mgr, _ := setupWith(t, p, instanceSpec)
	raw, nonce, _ := mgr.AuthorizeURL(context.Background(), &auth.User{Sub: "bob", Org: "acme"}, "sheets", "")
	au, _ := url.Parse(raw)
	p.challenge = au.Query().Get("code_challenge")
	if _, err := mgr.Callback(context.Background(), "good-code", au.Query().Get("state"), nonce); err == nil || !strings.Contains(err.Error(), "no instance_url") {
		t.Fatalf("missing field: %v", err)
	}
}

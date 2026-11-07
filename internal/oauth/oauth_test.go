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
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1",
					"token_type": "bearer", "expires_in": p.expiresIn, "scope": "sheets"})
			case "refresh_token":
				n := p.refreshes.Add(1)
				want := "refresh-" + string(rune('0'+n))
				if r.Form.Get("refresh_token") != want {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"stale refresh token"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + string(rune('1'+n)),
					"refresh_token": "refresh-" + string(rune('1'+n)), "expires_in": 3600})
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
	raw, err := mgr.AuthorizeURL(context.Background(), u, "sheets", "My sheets")
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
	c, err := mgr.Callback(context.Background(), "good-code", q.Get("state"))
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
	if _, err := mgr.Callback(context.Background(), "good-code", "garbage"); err == nil {
		t.Fatal("tampered state accepted")
	}
	u := &auth.User{Sub: "bob", Org: "acme"}
	raw, _ := mgr.AuthorizeURL(context.Background(), u, "sheets", "")
	au, _ := url.Parse(raw)
	p.challenge = au.Query().Get("code_challenge")
	if _, err := mgr.Callback(context.Background(), "bad-code", au.Query().Get("state")); err == nil ||
		!strings.Contains(err.Error(), "bad code") {
		t.Fatalf("bad code: %v", err)
	}
	// Expired state.
	mgr.now = func() time.Time { return time.Now().Add(stateTTL + time.Minute) }
	if _, err := mgr.Callback(context.Background(), "good-code", au.Query().Get("state")); err == nil {
		t.Fatal("expired state accepted")
	}
}

func TestAuthorizeNeedsAppCredentials(t *testing.T) {
	p := newProvider(t)
	mgr, _ := setup(t, p)
	mgr.settings = settings{}
	_, err := mgr.AuthorizeURL(context.Background(), &auth.User{Org: "acme"}, "sheets", "")
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("expected admin hint, got %v", err)
	}
}

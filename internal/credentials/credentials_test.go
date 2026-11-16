package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
)

// memStore stands in for PostgreSQL when G2F_TEST_DATABASE_URL is not set.
type memStore struct {
	mu    sync.Mutex
	conns map[string]store.Connection
}

func (m *memStore) CreateConnection(_ context.Context, c *store.Connection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.Kind == "" {
		c.Kind = store.KindOAuth2 // like the column default
	}
	m.conns[c.ID] = *c
	return nil
}

func (m *memStore) GetConnection(_ context.Context, id string) (*store.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &c, nil
}

func (m *memStore) UpdateConnectionFields(_ context.Context, c *store.Connection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.conns[c.ID]
	if !ok || cur.Kind != store.KindCredential {
		return store.ErrNotFound
	}
	cur.Fields, cur.SecretFields = c.Fields, c.SecretFields
	m.conns[c.ID] = cur
	return nil
}

func (m *memStore) ListConnections(_ context.Context, org string) ([]*store.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*store.Connection
	for _, c := range m.conns {
		if c.OrgID == org {
			c := c
			out = append(out, &c)
		}
	}
	return out, nil
}

type testStore interface {
	Store
	ListConnections(ctx context.Context, org string) ([]*store.Connection, error)
}

// stores returns the in-memory store and, if configured, PostgreSQL.
func stores(t *testing.T) map[string]testStore {
	out := map[string]testStore{"mem": &memStore{conns: map[string]store.Connection{}}}
	if url := os.Getenv("G2F_TEST_DATABASE_URL"); url != "" {
		ctx := context.Background()
		s, err := store.Open(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		out["postgres"] = s
	}
	return out
}

func registry(t *testing.T) *nodes.Registry {
	t.Helper()
	reg := nodes.NewRegistry()
	for _, raw := range []string{`
module: gh
name: GH
baseUrl: "{{ credential.apiUrl }}"
credentials:
  fields:
    - { name: token, type: secret, required: true }
    - { name: apiUrl, type: url, default: "https://api.example.com" }
    - { name: team }
  apiHosts: [api.example.com, "*.example.net"]
nodes: [{ type: get, name: Get, request: { path: /x } }]
`, `
module: hook
credentials:
  fields: [{ name: url, type: secretUrl, required: true }]
  apiHosts: [hooks.example.com]
nodes: [{ type: post, name: Post, request: { url: "{{ credential.url }}" } }]
`} {
		m, err := nodes.ParseManifest([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		list, err := nodes.HTTPModuleNodes(m, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.RegisterModule(m.Info("test"), list); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func TestCredentials(t *testing.T) {
	box, err := secrets.NewBox("ZGV2LWtleS1kZXYta2V5LWRldi1rZXktZGV2LWtleS0=", nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry(t)
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			org := "org-" + uuid.NewString()
			alice := &auth.User{Sub: "alice", Name: "Alice", Org: org}
			svc := NewService(st, box, reg)
			cleanup := func(c *store.Connection) {
				if s, ok := st.(*store.Store); ok && c != nil {
					t.Cleanup(func() { _ = s.DeleteConnection(ctx, c.ID) })
				}
			}

			// Validation against the manifest.
			for label, in := range map[string]map[string]any{
				"missing required": {"apiUrl": "https://api.example.com"},
				"unknown field":    {"token": "t", "nope": "x"},
				"not a string":     {"token": 42},
				"host not allowed": {"token": "t", "apiUrl": "https://evil.example.org"},
				"plain http":       {"token": "t", "apiUrl": "http://api.example.com"},
			} {
				if _, err := svc.Create(ctx, alice, "gh", "x", in); !errors.Is(err, ErrInvalid) {
					t.Errorf("%s: %v", label, err)
				}
			}
			if _, err := svc.Create(ctx, alice, "hook", "x", map[string]any{"url": "https://evil.example.org/services/x"}); !errors.Is(err, ErrInvalid) {
				t.Errorf("webhook on another host: %v", err)
			}
			if _, err := svc.Create(ctx, alice, "nope", "x", map[string]any{}); !errors.Is(err, ErrInvalid) {
				t.Errorf("unknown module: %v", err)
			}

			c, err := svc.Create(ctx, alice, "gh", "  ", map[string]any{"token": "ghp_first", "team": "core"})
			cleanup(c)
			if err != nil {
				t.Fatal(err)
			}
			if c.Name != "GH credential" || c.Kind != store.KindCredential || c.Scope != "private" || c.OwnerSub != "alice" {
				t.Fatalf("created: %+v", c)
			}
			hook, err := svc.Create(ctx, alice, "hook", "Alerts", map[string]any{"url": "https://hooks.example.com/services/T/B/X"})
			cleanup(hook)
			if err != nil {
				t.Fatal(err)
			}

			// Stored: secrets sealed, never serialized; non-secret fields readable.
			got, err := st.GetConnection(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != store.KindCredential || got.Fields["team"] != "core" || got.SecretFields["token"] == "" ||
				strings.Contains(got.SecretFields["token"], "ghp_first") {
				t.Fatalf("stored: %+v", got)
			}
			list, err := st.ListConnections(ctx, org)
			if err != nil || len(list) != 2 {
				t.Fatalf("list: %v %v", list, err)
			}
			raw, _ := json.Marshal(list)
			if strings.Contains(string(raw), "ghp_first") || strings.Contains(string(raw), got.SecretFields["token"]) ||
				strings.Contains(string(raw), "services/T/B/X") {
				t.Fatalf("list leaks secrets: %s", raw)
			}
			if set := SecretsSet(got); !set["token"] || len(set) != 1 {
				t.Fatalf("secrets set: %v", set)
			}

			// Resolution: defaults applied, secret values reported for scrubbing.
			values, secretValues, err := svc.Credential(ctx, org, "gh", c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if values["token"] != "ghp_first" || values["apiUrl"] != "https://api.example.com" || values["team"] != "core" ||
				len(secretValues) != 1 || secretValues[0] != "ghp_first" {
				t.Fatalf("values %v secrets %v", values, secretValues)
			}
			for label, args := range map[string][3]string{
				"other org":    {"org-other", "gh", c.ID},
				"other module": {org, "hook", c.ID},
				"missing":      {org, "gh", uuid.NewString()},
			} {
				if _, _, err := svc.Credential(ctx, args[0], args[1], args[2]); !errors.Is(err, ErrNotFound) {
					t.Errorf("%s: %v", label, err)
				}
			}

			// Update: omitted secrets and the stored placeholder keep their value, "" clears.
			if err := svc.Update(ctx, got, map[string]any{"apiUrl": "https://api.acme.example.net", "team": ""}); err != nil {
				t.Fatal(err)
			}
			if err := svc.Update(ctx, got, map[string]any{"token": nodes.SecretRef}); err != nil {
				t.Fatal(err)
			}
			values, _, _ = svc.Credential(ctx, org, "gh", c.ID)
			if values["token"] != "ghp_first" || values["apiUrl"] != "https://api.acme.example.net" || values["team"] != nil {
				t.Fatalf("after update: %v", values)
			}
			if err := svc.Update(ctx, got, map[string]any{"token": ""}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("clearing a required secret: %v", err)
			}
			if err := svc.Update(ctx, got, map[string]any{"token": "ghp_second"}); err != nil {
				t.Fatal(err)
			}
			if values, _, _ = svc.Credential(ctx, org, "gh", c.ID); values["token"] != "ghp_second" {
				t.Fatalf("replaced token: %v", values)
			}

			// AAD: a ciphertext moved to another connection or field does not decrypt.
			fresh, _ := st.GetConnection(ctx, c.ID)
			sealed := fresh.SecretFields["token"]
			other, err := svc.Create(ctx, alice, "gh", "other", map[string]any{"token": "ghp_other"})
			cleanup(other)
			if err != nil {
				t.Fatal(err)
			}
			other.SecretFields = map[string]string{"token": sealed}
			if err := st.UpdateConnectionFields(ctx, other); err != nil {
				t.Fatal(err)
			}
			if _, _, err := svc.Credential(ctx, org, "gh", other.ID); err == nil || !strings.Contains(err.Error(), "decrypt") {
				t.Fatalf("moved ciphertext: %v", err)
			}
			hookRow, _ := st.GetConnection(ctx, hook.ID)
			hookRow.SecretFields = map[string]string{"url": sealed}
			if err := st.UpdateConnectionFields(ctx, hookRow); err != nil {
				t.Fatal(err)
			}
			if _, _, err := svc.Credential(ctx, org, "hook", hook.ID); err == nil {
				t.Fatal("ciphertext moved to another field decrypted")
			}

			// OAuth2 accounts are not credentials.
			acct := &store.Connection{ID: uuid.NewString(), OrgID: org, Module: "gh", Name: "acct", OwnerSub: "alice", Scope: "private", AccessToken: "x"}
			if err := st.CreateConnection(ctx, acct); err != nil {
				t.Fatal(err)
			}
			cleanup(acct)
			if acct.Kind != store.KindOAuth2 {
				t.Fatalf("default kind %q", acct.Kind)
			}
			if err := svc.Update(ctx, acct, map[string]any{"token": "x"}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("update oauth account: %v", err)
			}
			if _, _, err := svc.Credential(ctx, org, "gh", acct.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("oauth account as credential: %v", err)
			}
		})
	}
}

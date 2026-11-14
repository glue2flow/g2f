package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glue2flow/g2f/internal/engine"
	"github.com/glue2flow/g2f/internal/netguard"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/store"
	"github.com/glue2flow/g2f/internal/workflow"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

const sampleDir = "../../examples/plugins/telegram"

// buildSample compiles the Go Telegram sample to WebAssembly once per test run.
var buildSample = sync.OnceValues(func() ([]byte, error) {
	out := filepath.Join(os.TempDir(), "g2f-telegram-test.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, ".")
	cmd.Dir = filepath.Join(sampleDir, "go")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, &buildError{string(b), err}
	}
	return os.ReadFile(out)
})

type buildError struct {
	out string
	err error
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

// sampleZip returns the sample bundle with allowedHosts replaced.
func sampleZip(t *testing.T, allowedHost string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a wasm plugin")
	}
	wasm, err := buildSample()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(sampleDir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.Replace(manifest, []byte("  - api.telegram.org"), []byte("  - "+allowedHost), 1)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string][]byte{"telegram/plugin.yaml": manifest, "telegram/plugin.wasm": wasm} {
		w, _ := zw.Create(name)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

type memStore struct {
	mu      sync.Mutex
	plugins map[string]*store.PluginRecord
	execs   map[string]store.Execution
	n       int
	saveErr error // returned by SavePlugin when set
}

func newMemStore() *memStore {
	return &memStore{plugins: map[string]*store.PluginRecord{}, execs: map[string]store.Execution{}}
}

func (m *memStore) ListPlugins(context.Context) ([]*store.PluginRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*store.PluginRecord
	for _, p := range m.plugins {
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) PluginDigests(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, p := range m.plugins {
		out[k] = p.SHA256
	}
	return out, nil
}

func (m *memStore) SavePlugin(_ context.Context, p *store.PluginRecord, replace bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	if _, ok := m.plugins[p.Module]; ok && !replace {
		return store.ErrPluginExists
	}
	c := *p
	c.InstalledAt = time.Now().UTC()
	p.InstalledAt = c.InstalledAt
	m.plugins[p.Module] = &c
	return nil
}

func (m *memStore) DeletePlugin(_ context.Context, module string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.plugins[module]; !ok {
		return store.ErrNotFound
	}
	delete(m.plugins, module)
	return nil
}

func (m *memStore) CreateExecution(_ context.Context, e *store.Execution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	e.ID = string(rune('a' + m.n))
	return nil
}

func (m *memStore) UpdateExecution(context.Context, *store.Execution) error { return nil }

// fakeSecrets plays the role of secrets.Service.
type fakeSecrets struct {
	node   map[string]map[string]string
	config map[string]any
	vars   map[string]any
}

func (f fakeSecrets) NodeSecrets(context.Context, string) (map[string]map[string]string, error) {
	return f.node, nil
}

func (f fakeSecrets) ModuleSettings(_ context.Context, _, module string) (map[string]any, map[string]any, error) {
	if module == "telegram" {
		return f.config, f.vars, nil
	}
	return map[string]any{}, map[string]any{}, nil
}

// fakeTelegram records which token was used per request.
func fakeTelegram(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		mu.Lock()
		tokens = append(tokens, parts[0])
		mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["chat_id"] == "bad" {
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
			"message_id": 7, "text": body["text"], "chat": map[string]any{"id": body["chat_id"]},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv, &tokens
}

func runTelegram(t *testing.T, reg *nodes.Registry, sec fakeSecrets, chatID string) *engine.Result {
	t.Helper()
	return runTelegramParams(t, reg, sec, chatID, map[string]any{
		"chatId": "{{ json.chat }}", "text": "Hi from {{ workflow.name }}",
	})
}

func runTelegramParams(t *testing.T, reg *nodes.Registry, sec fakeSecrets, chatID string, params map[string]any) *engine.Result {
	t.Helper()
	eng := engine.New(reg, newMemStore(), discard, engine.Options{Secrets: sec})
	wf := &workflow.Workflow{ID: "wf", Name: "Notify", OrgID: "acme", Graph: workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "t", Name: "Start", Type: "trigger.manual", Params: map[string]any{"payload": map[string]any{"chat": chatID}}},
			{ID: "tg", Name: "Telegram", Type: "telegram.sendMessage", Params: params},
		},
		Edges: []workflow.Edge{{ID: "e", Source: "t", Target: "tg"}},
	}}
	res, err := eng.Run(context.Background(), engine.Request{Workflow: wf, TriggerType: nodes.TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTelegramWasmPlugin(t *testing.T) {
	srv, tokens := fakeTelegram(t)
	reg := nodes.NewRegistry()
	if err := nodes.RegisterBuiltins(reg); err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	mgr := NewManager(reg, st, discard)

	info, err := mgr.Import(context.Background(), "telegram.zip", sampleZip(t, "127.0.0.1"), "alice", ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Runtime != "wasm" || len(info.Nodes) != 2 || len(info.Config) != 2 || len(info.Vars) != 1 {
		t.Fatalf("unexpected module info %+v", info)
	}

	cfg := map[string]any{"botToken": "org-default", "apiBaseUrl": srv.URL}

	// 1. Global config token.
	res := runTelegram(t, reg, fakeSecrets{config: cfg}, "42")
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("run failed: %s", res.Execution.Error)
	}
	if got := res.LastOutput[0]["text"]; got != "Hi from Notify" {
		t.Fatalf("unexpected output %v", res.LastOutput)
	}

	// 2. Per-node secret overrides the global token.
	res = runTelegram(t, reg, fakeSecrets{config: cfg, node: map[string]map[string]string{"tg": {"botToken": "users-own"}}}, "42")
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("run failed: %s", res.Execution.Error)
	}
	if want := []string{"org-default", "users-own"}; strings.Join(*tokens, ",") != strings.Join(want, ",") {
		t.Fatalf("tokens used: %v, want %v", *tokens, want)
	}

	// 3. API errors surface without leaking the token.
	res = runTelegram(t, reg, fakeSecrets{config: cfg}, "bad")
	if res.Execution.Status != store.StatusError || !strings.Contains(res.Execution.Error, "chat not found") {
		t.Fatalf("expected API error, got %q", res.Execution.Error)
	}
	if strings.Contains(res.Execution.Error, "org-default") {
		t.Fatal("token leaked into error")
	}

	// 4. No token at all.
	res = runTelegram(t, reg, fakeSecrets{config: map[string]any{"apiBaseUrl": srv.URL}}, "42")
	if !strings.Contains(res.Execution.Error, "no bot token") {
		t.Fatalf("expected missing token error, got %q", res.Execution.Error)
	}

	// 5. Vars are usable in node fields (Chat ID defaults to vars.defaultChatId);
	//    config is not: {{ config.botToken }} in a field evaluates to nothing.
	withVars := fakeSecrets{config: cfg, vars: map[string]any{"defaultChatId": "-100777"}}
	res = runTelegramParams(t, reg, withVars, "ignored", map[string]any{"text": "token=[{{ config.botToken }}]"})
	if res.Execution.Status != store.StatusSuccess {
		t.Fatalf("run failed: %s", res.Execution.Error)
	}
	out := res.LastOutput[0]
	if chat := out["chat"].(map[string]any)["id"]; chat != "-100777" {
		t.Fatalf("chat from vars = %v", chat)
	}
	if out["text"] != "token=[]" {
		t.Fatalf("config leaked into a node field: %v", out["text"])
	}

	// 6. Re-sync from the store on another "replica".
	reg2 := nodes.NewRegistry()
	_ = nodes.RegisterBuiltins(reg2)
	if err := NewManager(reg2, st, discard).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reg2.Exists("telegram.sendMessage") {
		t.Fatal("plugin not synced to second registry")
	}

	// 7. Delete.
	if err := mgr.Delete(context.Background(), "telegram"); err != nil {
		t.Fatal(err)
	}
	if reg.Exists("telegram.sendMessage") {
		t.Fatal("plugin still registered after delete")
	}
}

func TestWasmSandboxBlocksOtherHosts(t *testing.T) {
	srv, tokens := fakeTelegram(t)
	reg := nodes.NewRegistry()
	_ = nodes.RegisterBuiltins(reg)
	mgr := NewManager(reg, newMemStore(), discard)
	// Only api.telegram.org is allowed; the fake server on 127.0.0.1 is not.
	if _, err := mgr.Import(context.Background(), "telegram.zip", sampleZip(t, "api.telegram.org"), "alice", ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	res := runTelegram(t, reg, fakeSecrets{config: map[string]any{"botToken": "s3cr3t-token", "apiBaseUrl": srv.URL}}, "42")
	if res.Execution.Status != store.StatusError {
		t.Fatal("request to a non-allowed host must fail")
	}
	if len(*tokens) != 0 {
		t.Fatal("request reached the server")
	}
	if strings.Contains(res.Execution.Error, "s3cr3t-token") {
		t.Fatalf("token leaked into error: %s", res.Execution.Error)
	}
	if !strings.Contains(res.Execution.Error, "not allowed") {
		t.Fatalf("unexpected error: %s", res.Execution.Error)
	}
	t.Logf("sandbox error: %s", res.Execution.Error)
}

func TestParseBundleRejects(t *testing.T) {
	cases := map[string]string{
		"bad.txt":   "x",
		"wasm.yaml": "module: foo\nruntime: wasm\nnodes: [{type: a, name: A}]",
		"core.yaml": "module: core\nbaseUrl: http://x\nnodes: [{type: a, name: A}]",
		"typo.yaml": "module: foo\nbaseUrl: http://x\nnodez: []",
	}
	for name, data := range cases {
		if _, err := ParseBundle(name, []byte(data)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// The allowedHosts of a plugin also apply to every redirect hop.
func TestWasmRedirectLeavesAllowedHosts(t *testing.T) {
	_, tokens := fakeTelegram(t)
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// localhost is not in allowedHosts (127.0.0.1 is).
		http.Redirect(w, r, strings.Replace(target.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	reg := nodes.NewRegistry()
	_ = nodes.RegisterBuiltins(reg)
	mgr := NewManager(reg, newMemStore(), discard)
	if _, err := mgr.Import(context.Background(), "telegram.zip", sampleZip(t, "127.0.0.1"), "alice", ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	res := runTelegram(t, reg, fakeSecrets{config: map[string]any{"botToken": "tok", "apiBaseUrl": redirector.URL}}, "42")
	if res.Execution.Status != store.StatusError || !strings.Contains(res.Execution.Error, "not allowed by the plugin's allowedHosts") {
		t.Fatalf("expected the redirect to be refused, got %q", res.Execution.Error)
	}
	if reached.Load() != 0 || len(*tokens) != 0 {
		t.Fatal("redirect target was reached")
	}
}

// Plugin HTTP goes through the SSRF guard even for hosts in allowedHosts.
func TestWasmHTTPBlocksPrivateAddresses(t *testing.T) {
	srv, tokens := fakeTelegram(t)
	old := netguard.CurrentPolicy()
	netguard.SetPolicy(netguard.Policy{})
	t.Cleanup(func() { netguard.SetPolicy(old) })

	reg := nodes.NewRegistry()
	_ = nodes.RegisterBuiltins(reg)
	mgr := NewManager(reg, newMemStore(), discard)
	if _, err := mgr.Import(context.Background(), "telegram.zip", sampleZip(t, "127.0.0.1"), "alice", ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	res := runTelegram(t, reg, fakeSecrets{config: map[string]any{"botToken": "tok", "apiBaseUrl": srv.URL}}, "42")
	if res.Execution.Status != store.StatusError || !strings.Contains(res.Execution.Error, "private and local addresses are not allowed") {
		t.Fatalf("expected the guard to block, got %q", res.Execution.Error)
	}
	if len(*tokens) != 0 {
		t.Fatal("request reached the server")
	}
}
